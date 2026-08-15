package enactmodelinference

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
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
	"enact/internal/tools"
)

// defaultRetrievalTopK is how many RAG chunks are retrieved when the request
// does not set retrieval_top_k; maxRetrievalTopK bounds the override so a
// single request cannot pull an unbounded number of chunks into the prompt.
const (
	defaultRetrievalTopK = 5
	maxRetrievalTopK     = 50
)

// Ad-hoc context files become Bedrock DocumentBlocks: the Converse API
// allows at most 5 documents of ~4.5 MB each per request, and the model
// re-reads them (and the caller re-pays their tokens) on every request that
// carries them.
const (
	maxContextFiles     = 5
	maxContextFileBytes = 4_500_000
)

// documentFormats maps file extensions to the Converse document formats.
var documentFormats = map[string]string{
	".pdf": "pdf", ".csv": "csv", ".doc": "doc", ".docx": "docx",
	".xls": "xls", ".xlsx": "xlsx", ".html": "html", ".htm": "html",
	".txt": "txt", ".md": "md",
}

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
	tools      *tools.Client
	embedModel string
	// toolAuth resolves the caller's third-party credentials for tools that
	// declare requirements, and parks a call until they exist.
	toolAuth *toolAuthorizer
	// maxTurns caps how many tool-execution rounds one inference may run.
	maxTurns int
	logger   *logging.Logger
}

func newInferenceAPI(client *bedrock.Client, agentClient *agents.Client, rags *agents.RAGRepository, kbClient *kb.Client, toolsClient *tools.Client, toolAuth *toolAuthorizer, embedModel string, maxTurns int, logger *logging.Logger) *InferenceAPI {
	if maxTurns <= 0 {
		maxTurns = 10
	}
	return &InferenceAPI{
		client:     client,
		agents:     agentClient,
		rags:       rags,
		kb:         kbClient,
		tools:      toolsClient,
		toolAuth:   toolAuth,
		embedModel: embedModel,
		maxTurns:   maxTurns,
		logger:     logger,
	}
}

func (a *InferenceAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/inference").
		To(a.infer).
		Doc("Run a Bedrock inference request. context_files (base64 content, max 5 files of 4.5MB) are passed to the model natively as Converse DocumentBlocks on the last user message").
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
	documents, docErr := convertContextFiles(body)
	if docErr != "" {
		logger.Warn("invalid context files", "err", docErr, "files", len(body.ContextFiles))
		writeError(req, resp, http.StatusBadRequest, docErr)
		return
	}
	if len(documents) > 0 {
		names := make([]string, len(documents))
		total := 0
		for i, d := range documents {
			names[i] = d.Name + "." + d.Format
			total += len(d.Data)
		}
		logger.Info("context files accepted", "files", names, "total_bytes", total)
	}

	logger.Info("inference request decoded",
		"messages", len(body.Messages),
		"agent_id", body.AgentID,
		"retrieval_top_k", body.RetrievalTopK,
	)
	bedReq := toBedrockRequest(&body)
	bedReq.Documents = documents
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
	var bindings map[string]toolBinding
	if body.AgentID != "" {
		logger = logger.WithFields("agent_id", body.AgentID)
		logger.Info("Applying agent")
		var status int
		var msg string
		bindings, status, msg = a.applyAgent(req, logger, &body, bedReq)
		if status != 0 {
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

	// Rebuild the history now that the agent's tools are known: a past turn
	// that used tools is replayed as the tool_use/tool_result pair the model
	// originally saw, not just the sentence it ended with.
	bedReq.Messages = expandToolHistory(body.Messages, bindings, logger)

	if body.Stream {
		a.stream(req, resp, logger, bedReq, bindings)
		return
	}

	logger.Info("running inference", "messages", len(body.Messages))
	ctx := req.Request.Context()
	response := &InferenceResponse{Model: bedReq.Model}
	// The tool loop: each turn either finishes the response or requests
	// tool calls, whose results feed the next turn.
	for turn := 0; ; turn++ {
		out, err := a.client.Converse(ctx, bedReq)
		if err != nil {
			logger.Error("bedrock converse failed", "err", err)
			writeError(req, resp, http.StatusBadGateway, fmt.Sprintf("bedrock error: %v", err))
			return
		}
		response.StopReason = out.StopReason
		response.InputTokens += out.InputTokens
		response.OutputTokens += out.OutputTokens
		if out.StopReason != "tool_use" || len(out.ToolUses) == 0 {
			response.Content = out.Content
			break
		}
		if turn >= a.maxTurns {
			logger.Warn("max tool turns exceeded", "max_turns", a.maxTurns)
			writeError(req, resp, http.StatusBadGateway, fmt.Sprintf("tool loop exceeded the maximum of %d turns", a.maxTurns))
			return
		}
		logger.Info("model requested tool calls", "turn", turn+1, "calls", len(out.ToolUses))
		resultsMsg, records, err := a.executeToolUses(ctx, logger, turn+1, bindings, out.ToolUses, nil)
		if err != nil {
			logger.Error("tool execution failed", "err", err)
			writeError(req, resp, http.StatusBadGateway, fmt.Sprintf("tool execution failed: %v", err))
			return
		}
		response.ToolCalls = append(response.ToolCalls, records...)
		bedReq.Messages = append(bedReq.Messages,
			bedrock.Message{Role: "assistant", Content: out.Content, ToolUses: out.ToolUses},
			resultsMsg,
		)
	}
	logger.Info("inference completed",
		"stop_reason", response.StopReason,
		"tool_calls", len(response.ToolCalls),
		"input_tokens", response.InputTokens,
		"output_tokens", response.OutputTokens,
	)
	requesthelper.WriteJSON(req, resp, http.StatusOK, response)
}

// convertContextFiles validates a request's context files and converts them
// into Bedrock documents: count and per-file size caps, format derived from
// the filename extension, base64 decoding, name sanitization per Bedrock's
// document-name rules, and the requirement of a user message to carry them.
// A non-empty string return is the client-facing validation error.
func convertContextFiles(body InferenceRequest) ([]bedrock.Document, string) {
	if len(body.ContextFiles) == 0 {
		return nil, ""
	}
	if len(body.ContextFiles) > maxContextFiles {
		return nil, fmt.Sprintf("at most %d context files are allowed", maxContextFiles)
	}
	hasUser := false
	for _, m := range body.Messages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return nil, "context files require at least one user message to attach to"
	}

	docs := make([]bedrock.Document, 0, len(body.ContextFiles))
	for i, f := range body.ContextFiles {
		ext := strings.ToLower(filepath.Ext(f.Filename))
		format, ok := documentFormats[ext]
		if !ok {
			return nil, fmt.Sprintf("unsupported context file type %q (supported: pdf, csv, doc, docx, xls, xlsx, html, txt, md)", ext)
		}
		data, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, fmt.Sprintf("context file %q content is not valid base64", f.Filename)
		}
		if len(data) == 0 {
			return nil, fmt.Sprintf("context file %q is empty", f.Filename)
		}
		if len(data) > maxContextFileBytes {
			return nil, fmt.Sprintf("context file %q exceeds the %d-byte limit", f.Filename, maxContextFileBytes)
		}
		docs = append(docs, bedrock.Document{
			Name:   sanitizeDocumentName(f.Filename, i),
			Format: format,
			Data:   data,
		})
	}
	return docs, ""
}

// sanitizeDocumentName maps a filename onto Bedrock's document-name rules:
// only alphanumerics, single spaces, hyphens, parentheses and brackets.
func sanitizeDocumentName(filename string, index int) string {
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	var b strings.Builder
	lastSpace := false
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '(' || r == ')' || r == '[' || r == ']':
			b.WriteRune(r)
			lastSpace = false
		case r == ' ' || r == '_' || r == '.':
			if !lastSpace {
				b.WriteRune(' ')
			}
			lastSpace = true
		}
	}
	name := strings.TrimSpace(b.String())
	if name == "" {
		name = fmt.Sprintf("document-%d", index+1)
	}
	return name
}

// applyAgent loads the agent referenced by body.AgentID, resolves its model,
// augments bedReq's system prompt with (1) the full text of every document
// in the agent's knowledge bases and (2) chunks retrieved from the agent's
// RAG collection, and advertises the agent's MCP tools. It returns the tool
// bindings and (0, "") on success, or an HTTP status and message on failure.
func (a *InferenceAPI) applyAgent(req *restful.Request, logger *logging.Logger, body *InferenceRequest, bedReq *bedrock.ConverseRequest) (map[string]toolBinding, int, string) {
	ctx := req.Request.Context()
	agent, found, err := a.agents.Get(ctx, body.AgentID)
	if err != nil {
		logger.Error("failed to load agent", "err", err)
		return nil, http.StatusBadGateway, "failed to load agent"
	}
	logger = logger.WithFields(
		"agent_id", agent.ID,
		"model", agent.Model,
	)
	if !found {
		logger.Warn("agent not found")
		return nil, http.StatusNotFound, fmt.Sprintf("agent %q not found", body.AgentID)
	}
	logger.Info("agent loaded", "knowledge_base_ids", agent.KnowledgeBaseIDs, "tools", agent.Tools, "system_prompt_chars", len(agent.SystemPrompt))

	modelID, ok := models.Resolve(agent.Model)
	if !ok {
		logger.Error("agent references unknown model", "model", agent.Model)
		return nil, http.StatusInternalServerError, fmt.Sprintf("agent references unknown model %q", agent.Model)
	}
	bedReq.Model = modelID
	logger.Info("model resolved", "model_id", modelID)

	// MCP tools: resolve the agent's server list against the registry's
	// cache into Bedrock tool specs plus the execution bindings.
	var bindings map[string]toolBinding
	if len(agent.Tools) > 0 {
		specs, b, err := a.buildToolBindings(ctx, logger, agent.Tools)
		if err != nil {
			logger.Error("failed to build tool bindings", "err", err)
			return nil, http.StatusBadGateway, "failed to resolve agent tools"
		}
		bedReq.Tools = specs
		bindings = b
	}

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
				return nil, http.StatusBadGateway, "failed to load knowledge-base documents"
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
		return nil, http.StatusBadGateway, "failed to check RAG collection"
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
			return nil, http.StatusBadGateway, "failed to embed query for retrieval"
		}
		logger.Info("query embedded", "dimensions", len(vector))
		retrieved, err := a.rags.Search(ctx, agent.UserID, agent.ID, vector, topK)
		if err != nil {
			logger.Error("failed to retrieve rag context", "err", err)
			return nil, http.StatusBadGateway, "failed to retrieve RAG context"
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
	return bindings, 0, ""
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

func (a *InferenceAPI) stream(req *restful.Request, resp *restful.Response, logger *logging.Logger, bedReq *bedrock.ConverseRequest, bindings map[string]toolBinding) {
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
	// emit publishes a named SSE event (toolCall / toolCallResult): plain
	// data-chunk consumers never see named events, so the delta protocol is
	// unchanged for clients that ignore tools.
	emit := func(event string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			logger.Error("failed to marshal event", "event", event, "err", err)
			return err
		}
		if _, err := fmt.Fprintf(resp, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	fail := func(err error) {
		if errors.Is(err, req.Request.Context().Err()) {
			logger.Info("streaming inference cancelled by client", "err", err)
			return
		}
		logger.Error("streaming inference failed", "err", err)
		// The error event carries the meta block (fresh, so execution_time_ms
		// reflects when the failure happened): a user reporting a mid-stream
		// failure has the trace id right next to the error.
		errBody, _ := json.Marshal(map[string]any{
			"error": err.Error(),
			"meta":  requesthelper.MetaFrom(req.Request.Context()),
		})
		_, _ = fmt.Fprintf(resp, "event: error\ndata: %s\n\n", errBody)
		flusher.Flush()
	}

	// The tool loop: each Bedrock turn streams its text deltas live; a
	// tool_use stop executes the requested calls (announced via toolCall /
	// toolCallResult events) and feeds their results into the next turn.
	ctx := req.Request.Context()
	for turn := 0; ; turn++ {
		result, err := a.client.ConverseStream(ctx, bedReq, send)
		if err != nil {
			fail(err)
			return
		}
		if result.StopReason != "tool_use" || len(result.ToolUses) == 0 {
			break
		}
		if turn >= a.maxTurns {
			fail(fmt.Errorf("tool loop exceeded the maximum of %d turns", a.maxTurns))
			return
		}
		logger.Info("model requested tool calls", "turn", turn+1, "calls", len(result.ToolUses))
		resultsMsg, _, err := a.executeToolUses(ctx, logger, turn+1, bindings, result.ToolUses, emit)
		if err != nil {
			fail(err)
			return
		}
		bedReq.Messages = append(bedReq.Messages,
			bedrock.Message{Role: "assistant", Content: result.Content, ToolUses: result.ToolUses},
			resultsMsg,
		)
	}
	// End-of-response marker, previously emitted by the bedrock client;
	// with tool turns in play only the handler knows when it is over.
	_ = send(bedrock.StreamChunk{Done: true})
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
