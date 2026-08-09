package enactmain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/inference"
	"enact/internal/logging"
	"enact/internal/requesthelper"
)

// testInferenceRequest is a one-shot invocation: the "test agent" surface.
// Unlike conversation messages, nothing is persisted — the UI uses this to
// try an agent (or raw model) out.
type testInferenceRequest struct {
	AgentID       string                  `json:"agent_id,omitempty"`
	Model         string                  `json:"model,omitempty"`
	Messages      []inference.Message     `json:"messages"`
	RetrievalTopK *int                    `json:"retrieval_top_k,omitempty"`
	ContextFiles  []inference.ContextFile `json:"context_files,omitempty"`
}

// inferenceWebService returns the session-guarded one-shot inference route.
func (a *MainAPI) inferenceWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/inference")
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.POST("").
		To(a.testInference).
		Consumes(restful.MIME_JSON).
		Reads(testInferenceRequest{}).
		Doc("One-shot inference for trying out an agent or model: streams the reply as SSE; nothing is persisted").
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	return ws
}

// testInference relays a one-shot inference call as SSE. The session user's
// identity travels downstream, so agent and KB resolution are scoped to
// their owner exactly as conversation messages are.
func (a *MainAPI) testInference(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("test inference requested")

	var body testInferenceRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid test inference body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	attachments := make([]string, 0, len(body.ContextFiles))
	for _, f := range body.ContextFiles {
		attachments = append(attachments, f.Filename)
	}
	logger.Info("test inference decoded",
		"agent_id", body.AgentID, "model", body.Model, "messages", len(body.Messages),
		"retrieval_top_k", body.RetrievalTopK, "attachments", attachments)

	if len(body.Messages) == 0 {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "messages must not be empty")
		return
	}
	hasContent := false
	for _, m := range body.Messages {
		if strings.TrimSpace(m.Content) != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "messages must carry content")
		return
	}
	if (body.AgentID == "") == (body.Model == "") {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "exactly one of agent_id or model must be set")
		return
	}
	if len(body.ContextFiles) > maxMessageContextFiles {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("at most %d context files are allowed", maxMessageContextFiles))
		return
	}

	infReq := inference.Request{
		AgentID:       body.AgentID,
		Model:         body.Model,
		Messages:      body.Messages,
		RetrievalTopK: body.RetrievalTopK,
		ContextFiles:  body.ContextFiles,
	}
	assistant, streamErr := a.relayInferenceStream(req, resp, logger, infReq, map[string]any{"mode": "test"})
	if streamErr != nil {
		logger.Warn("test inference stream ended with error", "err", streamErr, "assistant_chars", len(assistant))
		return
	}
	logger.Info("test inference completed", "assistant_chars", len(assistant))
}

// relayInferenceStream streams an inference call back to the browser as SSE:
// an initial `meta` event (trace id plus extras), relayed `data` chunks, and
// an `error` event on failure. It returns the accumulated assistant text and
// the upstream error, if any. Shared by conversation messages and the
// one-shot test endpoint.
func (a *MainAPI) relayInferenceStream(req *restful.Request, resp *restful.Response, logger *logging.Logger, infReq inference.Request, metaExtra map[string]any) (string, error) {
	flusher, ok := resp.ResponseWriter.(http.Flusher)
	if !ok {
		logger.Error("streaming not supported by underlying writer")
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "streaming not supported")
		return "", fmt.Errorf("streaming not supported by underlying writer")
	}
	meta := requesthelper.MetaFrom(req.Request.Context())
	resp.Header().Set("Content-Type", "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("X-Accel-Buffering", "no")
	resp.Header().Set("X-Trace-Id", meta.TraceID)
	resp.WriteHeader(http.StatusOK)

	metaEvent := map[string]any{"trace_id": meta.TraceID}
	for k, v := range metaExtra {
		metaEvent[k] = v
	}
	if metaBody, err := json.Marshal(metaEvent); err == nil {
		_, _ = fmt.Fprintf(resp, "event: meta\ndata: %s\n\n", metaBody)
		flusher.Flush()
	}

	var assistant strings.Builder
	streamErr := a.inference.Stream(req.Request.Context(), infReq, func(ev inference.StreamEvent) error {
		switch ev.Event {
		case "message":
			var chunk struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err == nil && chunk.Delta != "" {
				assistant.WriteString(chunk.Delta)
			}
			_, err := fmt.Fprintf(resp, "data: %s\n\n", ev.Data)
			flusher.Flush()
			return err
		case "error":
			_, err := fmt.Fprintf(resp, "event: error\ndata: %s\n\n", ev.Data)
			flusher.Flush()
			return err
		default:
			// Upstream meta is not relayed: this response has its own meta
			// event of the same trace.
			return nil
		}
	})
	if streamErr != nil {
		logger.Error("inference stream failed", "err", streamErr, "assistant_chars", assistant.Len())
		errBody, _ := json.Marshal(map[string]any{"error": streamErr.Error(), "meta": requesthelper.MetaFrom(req.Request.Context())})
		_, _ = fmt.Fprintf(resp, "event: error\ndata: %s\n\n", errBody)
		flusher.Flush()
	}
	return assistant.String(), streamErr
}
