package enactmodelinference

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"
)

// InferenceAPI provides endpoints for running inference requests.
type InferenceAPI struct {
	client *bedrockClient
}

func newInferenceAPI(client *bedrockClient) *InferenceAPI {
	return &InferenceAPI{client: client}
}

func (a *InferenceAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/inference").
		To(a.infer).
		Doc("Run a Bedrock inference request").
		Reads(InferenceRequest{}).
		Returns(http.StatusOK, "OK", InferenceResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusBadGateway, "Upstream Bedrock error", errorResponse{}))

	return ws
}

func (a *InferenceAPI) infer(req *restful.Request, resp *restful.Response) {
	var body InferenceRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.Model == "" {
		writeError(resp, http.StatusBadRequest, "model is required")
		return
	}
	if len(body.Messages) == 0 {
		writeError(resp, http.StatusBadRequest, "messages must not be empty")
		return
	}

	if body.Stream {
		a.stream(req, resp, &body)
		return
	}

	result, err := a.client.Converse(req.Request.Context(), &body)
	if err != nil {
		log.Printf("bedrock converse: %v", err)
		writeError(resp, http.StatusBadGateway, fmt.Sprintf("bedrock error: %v", err))
		return
	}
	_ = resp.WriteAsJson(result)
}

func (a *InferenceAPI) stream(req *restful.Request, resp *restful.Response, body *InferenceRequest) {
	flusher, ok := resp.ResponseWriter.(http.Flusher)
	if !ok {
		writeError(resp, http.StatusInternalServerError, "streaming not supported by underlying writer")
		return
	}

	resp.Header().Set("Content-Type", "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("Connection", "keep-alive")
	resp.Header().Set("X-Accel-Buffering", "no")
	resp.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(chunk StreamChunk) error {
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(resp, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := a.client.ConverseStream(req.Request.Context(), body, send); err != nil {
		if errors.Is(err, req.Request.Context().Err()) {
			return
		}
		log.Printf("bedrock converse stream: %v", err)
		errBody, _ := json.Marshal(errorResponse{Error: err.Error()})
		_, _ = fmt.Fprintf(resp, "event: error\ndata: %s\n\n", errBody)
		flusher.Flush()
	}
}

func writeError(resp *restful.Response, status int, msg string) {
	_ = resp.WriteHeaderAndJson(status, errorResponse{Error: msg}, restful.MIME_JSON)
}
