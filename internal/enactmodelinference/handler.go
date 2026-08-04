package enactmodelinference

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/bedrock"
	"enact/internal/identity"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/rag"
	"enact/internal/requesthelper"
)

// defaultRetrievalTopK is how many RAG chunks are retrieved when the request
// does not set retrieval_top_k; maxRetrievalTopK bounds the override so a
// single request cannot pull an unbounded number of chunks into the prompt.
const (
	defaultRetrievalTopK = 5
	maxRetrievalTopK     = 50
)

// InferenceAPI provides endpoints for running inference requests. When a
// request carries an agent_id, the API resolves the agent's model and system
// prompt and augments the prompt two ways, each independent of the other:
//
//   - Context files: every document of every knowledge base the agent
//     references is loaded whole into the prompt.
//   - RAG: the agent's own RAG collection (uploaded separately) is searched
//     by embedding the query, and the top-k chunks are added.
type InferenceAPI struct {
	client     *bedrock.Client
	agents     *agents.Client
	rags       *agents.RAGRepository
	kb         *kb.Client
	embedModel string
	logger     *logging.Logger
}

func newInferenceAPI(client *bedrock.Client, agentClient *agents.Client, rags *agents.RAGRepository, kbClient *kb.Client, embedModel string, logger *logging.Logger) *InferenceAPI {
	return &InferenceAPI{client: client, agents: agentClient, rags: rags, kb: kbClient, embedModel: embedModel, logger: logger}
}

func (a *InferenceAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1").
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
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("inference requested")
	var body InferenceRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid inference request body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if len(body.Messages) == 0 {
		logger.Warn("inference request has no messages")
		writeError(req, resp, http.StatusBadRequest, "messages must not be empty")
		return
	}
	if body.RetrievalTopK != nil && (*body.RetrievalTopK < 1 || *body.RetrievalTopK > maxRetrievalTopK) {
		logger.Warn("inference request has invalid retrieval_top_k", "retrieval_top_k", *body.RetrievalTopK)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("retrieval_top_k must be between 1 and %d", maxRetrievalTopK))
		return
	}

	logger.Info("inference request decoded",
		"messages", len(body.Messages),
		"agent_id", body.AgentID,
		"retrieval_top_k", body.RetrievalTopK,
	)
	bedReq := toBedrockRequest(&body)
	logger = logger.WithFields(
		"model", bedReq.Model,
		"top_p", bedReq.TopP,
		"temperature", bedReq.Temperature,
		"max_tokens", bedReq.MaxTokens,
		"stream", body.Stream,
	)

	// When an agent is named, override model/system prompt from the agent and
	// retrieve knowledge-base context. Otherwise the caller must supply a
	// friendly model name, which is resolved against the registry the same way
	// agent.Model is.
	if body.AgentID != "" {
		logger = logger.WithFields("agent_id", body.AgentID)
		logger.Info("Applying agent")
		if status, msg := a.applyAgent(req, logger, &body, bedReq); status != 0 {
			writeError(req, resp, status, msg)
			return
		}
	} else {
		if body.Model == "" {
			logger.Warn("inference request has neither agent_id nor model")
			writeError(req, resp, http.StatusBadRequest, "model is required when agent_id is not set")
			return
		}
		modelID, ok := models.Resolve(body.Model)
		if !ok {
			logger.Warn("inference request references unknown model", "model", body.Model)
			writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("unknown model %q", body.Model))
			return
		}
		bedReq.Model = modelID
	}
	logger = logger.WithFields("model", bedReq.Model)

	if body.Stream {
		a.stream(req, resp, logger, bedReq)
		return
	}

	logger.Info("running inference", "messages", len(body.Messages))
	out, err := a.client.Converse(req.Request.Context(), bedReq)
	if err != nil {
		logger.Error("bedrock converse failed", "err", err)
		writeError(req, resp, http.StatusBadGateway, fmt.Sprintf("bedrock error: %v", err))
		return
	}
	logger.Info("inference completed",
		"stop_reason", out.StopReason,
		"input_tokens", out.InputTokens,
		"output_tokens", out.OutputTokens,
	)
	requesthelper.WriteJSON(req, resp, http.StatusOK, &InferenceResponse{
		Model:        bedReq.Model,
		Content:      out.Content,
		StopReason:   out.StopReason,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
	})
}

// applyAgent loads the agent referenced by body.AgentID, resolves its model,
// and augments bedReq's system prompt with (1) the full text of every
// document in the agent's knowledge bases and (2) chunks retrieved from the
// agent's RAG collection. It returns (0, "") on success, or an HTTP status
// and message on failure.
func (a *InferenceAPI) applyAgent(req *restful.Request, logger *logging.Logger, body *InferenceRequest, bedReq *bedrock.ConverseRequest) (int, string) {
	ctx := req.Request.Context()
	agent, found, err := a.agents.Get(ctx, body.AgentID)
	if err != nil {
		logger.Error("failed to load agent", "err", err)
		return http.StatusBadGateway, "failed to load agent"
	}
	logger = logger.WithFields(
		"agent_id", agent.ID,
		"model", agent.Model,
	)
	if !found {
		logger.Warn("agent not found")
		return http.StatusNotFound, fmt.Sprintf("agent %q not found", body.AgentID)
	}
	logger.Info("agent loaded", "knowledge_base_ids", agent.KnowledgeBaseIDs, "system_prompt_chars", len(agent.SystemPrompt))

	modelID, ok := models.Resolve(agent.Model)
	if !ok {
		logger.Error("agent references unknown model", "model", agent.Model)
		return http.StatusInternalServerError, fmt.Sprintf("agent references unknown model %q", agent.Model)
	}
	bedReq.Model = modelID
	logger.Info("model resolved", "model_id", modelID)

	var blocks []string

	// Context files: fetch every referenced KB's documents from the KB
	// service, impersonating the agent's owner — their KBs are what the agent
	// was configured with, regardless of who invokes it.
	if len(agent.KnowledgeBaseIDs) > 0 {
		ownerCtx := identity.WithUserID(ctx, agent.UserID)
		var files []rag.File
		totalChars := 0
		for _, kbID := range agent.KnowledgeBaseIDs {
			docs, found, err := a.kb.ListDocuments(ownerCtx, kbID)
			if err != nil {
				logger.Error("failed to load context documents", "kb_id", kbID, "err", err)
				return http.StatusBadGateway, "failed to load knowledge-base documents"
			}
			if !found {
				// The KB was deleted after the agent was created; degrade to
				// no context rather than failing the inference.
				logger.Warn("agent references missing knowledge base", "kb_id", kbID)
				continue
			}
			for _, d := range docs {
				files = append(files, rag.File{Name: d.Filename, Text: d.Text})
				totalChars += len(d.Text)
			}
		}
		logger.Info("context documents loaded", "files", len(files), "total_chars", totalChars)
		if block := rag.FormatFiles(files); block != "" {
			blocks = append(blocks, block)
		}
	}

	// RAG: retrieve from the agent's own collection using the query embedding.
	// The (cheap) emptiness check runs first so agents without RAG documents
	// never pay for a query embedding.
	hasRAG, err := a.rags.HasChunks(ctx, agent.UserID, agent.ID)
	if err != nil {
		logger.Error("failed to check rag collection", "err", err)
		return http.StatusBadGateway, "failed to check RAG collection"
	}
	logger.Info("rag collection checked", "has_chunks", hasRAG)
	if query := lastUserMessage(body.Messages); hasRAG && query != "" {
		topK := defaultRetrievalTopK
		if body.RetrievalTopK != nil {
			topK = *body.RetrievalTopK
		}
		vector, err := a.client.Embed(ctx, a.embedModel, query)
		if err != nil {
			logger.Error("failed to embed retrieval query", "err", err)
			return http.StatusBadGateway, "failed to embed query for retrieval"
		}
		logger.Info("query embedded", "dimensions", len(vector))
		retrieved, err := a.rags.Search(ctx, agent.UserID, agent.ID, vector, topK)
		if err != nil {
			logger.Error("failed to retrieve rag context", "err", err)
			return http.StatusBadGateway, "failed to retrieve RAG context"
		}
		logger.Info("rag context retrieved", "chunks", len(retrieved), "top_k", topK)
		texts := make([]string, len(retrieved))
		for i, c := range retrieved {
			texts[i] = c.Text
		}
		if block := rag.FormatContext(texts); block != "" {
			blocks = append(blocks, block)
		}
	}

	if agent.SystemPrompt != "" {
		blocks = append(blocks, agent.SystemPrompt)
	}
	bedReq.SystemPrompt = strings.Join(blocks, "\n\n")
	logger.Info("system prompt assembled", "blocks", len(blocks), "total_chars", len(bedReq.SystemPrompt))
	return 0, ""
}

// lastUserMessage returns the content of the last user-role message, falling
// back to the last message of any role.
func lastUserMessage(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

func (a *InferenceAPI) stream(req *restful.Request, resp *restful.Response, logger *logging.Logger, bedReq *bedrock.ConverseRequest) {
	flusher, ok := resp.ResponseWriter.(http.Flusher)
	if !ok {
		logger.Error("streaming not supported by underlying writer")
		writeError(req, resp, http.StatusInternalServerError, "streaming not supported by underlying writer")
		return
	}
	logger.Info("streaming inference started")
	meta := requesthelper.MetaFrom(req.Request.Context())

	resp.Header().Set("Content-Type", "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("Connection", "keep-alive")
	resp.Header().Set("X-Accel-Buffering", "no")
	// Trace id in a header for clients that can read headers (curl, fetch)...
	resp.Header().Set("X-Trace-Id", meta.TraceID)
	resp.WriteHeader(http.StatusOK)
	flusher.Flush()

	// ...and as the first SSE event for those that cannot: the browser
	// EventSource API never exposes response headers, so in-band is the only
	// way a browser client can capture the trace id. A named event is
	// invisible to plain `data:` message handlers, so consumers that only
	// read chunks are unaffected.
	if metaBody, err := json.Marshal(meta); err == nil {
		_, _ = fmt.Fprintf(resp, "event: meta\ndata: %s\n\n", metaBody)
		flusher.Flush()
	}

	send := func(chunk bedrock.StreamChunk) error {
		b, err := json.Marshal(chunk)
		if err != nil {
			logger.Error("failed to marshal chunk", "err", err)
			return err
		}
		if _, err := fmt.Fprintf(resp, "data: %s\n\n", b); err != nil {
			logger.Error("failed to write chunk", "err", err)
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := a.client.ConverseStream(req.Request.Context(), bedReq, send); err != nil {
		if errors.Is(err, req.Request.Context().Err()) {
			logger.Info("streaming inference cancelled by client", "err", err)
			return
		}
		logger.Error("bedrock converse stream failed", "err", err)
		// The error event carries the meta block (fresh, so execution_time_ms
		// reflects when the failure happened): a user reporting a mid-stream
		// failure has the trace id right next to the error.
		errBody, _ := json.Marshal(map[string]any{
			"error": err.Error(),
			"meta":  requesthelper.MetaFrom(req.Request.Context()),
		})
		_, _ = fmt.Fprintf(resp, "event: error\ndata: %s\n\n", errBody)
		flusher.Flush()
		return
	}
	logger.Info("streaming inference completed")
}

func toBedrockRequest(body *InferenceRequest) *bedrock.ConverseRequest {
	msgs := make([]bedrock.Message, len(body.Messages))
	for i, m := range body.Messages {
		msgs[i] = bedrock.Message{Role: m.Role, Content: m.Content}
	}
	return &bedrock.ConverseRequest{
		Model:        body.Model,
		Messages:     msgs,
		SystemPrompt: body.SystemPrompt,
		MaxTokens:    body.MaxTokens,
		Temperature:  body.Temperature,
		TopP:         body.TopP,
	}
}

func writeError(req *restful.Request, resp *restful.Response, status int, msg string) {
	requesthelper.WriteError(req, resp, status, msg)
}
