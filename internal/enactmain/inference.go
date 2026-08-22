package enactmain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/conversations"
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
	// This route streams SSE: without text/event-stream in Produces,
	// go-restful answers 406 to a client that declares it accepts one.
	ws.Path("/inference").Produces(restful.MIME_JSON, "text/event-stream")
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireCaller)

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
	out, streamErr := a.relayInferenceStream(req, resp, logger, infReq, map[string]any{"mode": "test"})
	if streamErr != nil {
		logger.Warn("test inference stream ended with error", "err", streamErr, "assistant_chars", len(out.Assistant))
		return
	}
	logger.Info("test inference completed", "assistant_chars", len(out.Assistant), "tool_calls", len(out.ToolCalls))
}

// relayInferenceStream streams an inference call back to the browser as SSE:
// an initial `meta` event (trace id plus extras), relayed `data` chunks, and
// an `error` event on failure. It returns the accumulated assistant text and
// the upstream error, if any. Shared by conversation messages and the
// one-shot test endpoint.
// relayed is what a relayed stream produced, for persisting afterwards: the
// assistant's text and the tool calls it made.
type relayed struct {
	Assistant string
	ToolCalls []conversations.ToolCall
}

func (a *MainAPI) relayInferenceStream(req *restful.Request, resp *restful.Response, logger *logging.Logger, infReq inference.Request, metaExtra map[string]any) (relayed, error) {
	flusher, ok := resp.ResponseWriter.(http.Flusher)
	if !ok {
		logger.Error("streaming not supported by underlying writer")
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "streaming not supported")
		return relayed{}, fmt.Errorf("streaming not supported by underlying writer")
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
	// Tool calls are collected as they stream, keyed by tool_use_id so the
	// result event can find the call it belongs to.
	var toolCalls []conversations.ToolCall
	byUseID := map[string]int{}
	// Text arrives before the round that follows it, so the assistant's
	// words are split at each round boundary: what came before a round
	// belongs to that round, and what is left at the end is the answer.
	// Without this the record would concatenate "let me check" and the
	// conclusion into one string that was never said in one breath.
	flushed, currentTurn := 0, 0
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
		case "toolCall", "toolCallResult", "toolCallAuthorizationRequired":
			// Tool-loop progress from the inference service, forwarded
			// verbatim so the frontend can render live tool activity — and,
			// when a call is refused for want of a credential, prompt the
			// user to connect the account it needed.
			//
			// The call and its result are also collected, so reopening the
			// conversation shows what the agent did. The authorization event
			// is not: the refusal already reaches the record as the call's
			// error result. A parse failure only costs the record, never the
			// relay — the client still gets the bytes.
			switch ev.Event {
			case "toolCall":
				var call conversations.ToolCall
				if err := json.Unmarshal([]byte(ev.Data), &call); err != nil {
					logger.Warn("could not record a tool call", "err", err)
				} else {
					if call.Turn != currentTurn {
						call.Text = assistant.String()[flushed:]
						flushed = assistant.Len()
						currentTurn = call.Turn
					}
					byUseID[call.ToolUseID] = len(toolCalls)
					toolCalls = append(toolCalls, call)
				}
			case "toolCallResult":
				var result conversations.ToolCall
				if err := json.Unmarshal([]byte(ev.Data), &result); err != nil {
					logger.Warn("could not record a tool result", "err", err)
				} else if i, ok := byUseID[result.ToolUseID]; ok {
					toolCalls[i].Content = result.Content
					toolCalls[i].StructuredContent = result.StructuredContent
					toolCalls[i].IsError = result.IsError
				} else {
					// A result with no call announced: keep it rather than
					// lose the fact that the tool ran.
					toolCalls = append(toolCalls, result)
				}
			}
			_, err := fmt.Fprintf(resp, "event: %s\ndata: %s\n\n", ev.Event, ev.Data)
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
	// Only what followed the last round is the answer; earlier text is on
	// the round it preceded.
	return relayed{Assistant: assistant.String()[flushed:], ToolCalls: toolCalls}, streamErr
}
