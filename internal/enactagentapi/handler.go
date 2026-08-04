package enactagentapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/agents"
	"enact/internal/identity"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/queue"
	"enact/internal/requesthelper"
)

// maxUploadBytes caps the size of a RAG document upload. Requests whose body
// exceeds this are rejected before being read into memory.
const maxUploadBytes = 50 << 20 // 50 MiB

// fileFormField is the multipart form field carrying the document file.
const fileFormField = "file"

// AgentAPI exposes agent CRUD and RAG document upload. It uses the agent
// repository for the CRUD, the KB service client to validate the knowledge
// bases an agent references (a live call to enact-kb-api), the RAG repository
// to cascade-delete an agent's RAG collection, and the queue producer to hand
// RAG documents to the indexer.
type AgentAPI struct {
	agents   *agents.Repository
	rags     *agents.RAGRepository
	kbs      *kb.Client
	producer *queue.Producer
	logger   *logging.Logger
}

func newAgentAPI(agentRepo *agents.Repository, rags *agents.RAGRepository, kbs *kb.Client, producer *queue.Producer, logger *logging.Logger) *AgentAPI {
	return &AgentAPI{agents: agentRepo, rags: rags, kbs: kbs, producer: producer, logger: logger}
}

type agentRequest struct {
	Model            string   `json:"model"`
	SystemPrompt     string   `json:"system_prompt"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
}

// agentUpdateRequest is the partial-update body: pointer fields distinguish
// "absent — leave unchanged" (nil) from "provided — set to this value",
// including explicit clears ("" for the prompt, [] for the KB list). A JSON
// null is indistinguishable from an absent field and also means "unchanged".
type agentUpdateRequest struct {
	Model            *string   `json:"model"`
	SystemPrompt     *string   `json:"system_prompt"`
	KnowledgeBaseIDs *[]string `json:"knowledge_base_ids"`
}

type agentResponse struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	Model            string    `json:"model"`
	SystemPrompt     string    `json:"system_prompt"`
	KnowledgeBaseIDs []string  `json:"knowledge_base_ids"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type listAgentsResponse struct {
	Agents []agentResponse `json:"agents"`
}

// uploadRAGDocumentResponse acknowledges a multi-file upload: one entry per
// file received, in upload order.
type uploadRAGDocumentResponse struct {
	AgentID   string           `json:"agent_id"`
	Documents []queuedDocument `json:"documents"`
}

type queuedDocument struct {
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Status     string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (a *AgentAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/agents").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("").
		To(a.create).
		Reads(agentRequest{}).
		Doc("Create an agent").
		Returns(http.StatusCreated, "Created", agentResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.list).
		Doc("List the caller's agents").
		Returns(http.StatusOK, "OK", listAgentsResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.get).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Get an agent").
		Returns(http.StatusOK, "OK", agentResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.update).
		Param(ws.PathParameter("id", "agent id")).
		Reads(agentUpdateRequest{}).
		Doc("Partially update an agent: only provided fields change, absent fields keep their value (clear with \"\" / [])").
		Returns(http.StatusOK, "OK", agentResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.delete).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Delete an agent and its RAG collection").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/rag/documents").
		To(a.uploadRAGDocument).
		Consumes("multipart/form-data").
		Param(ws.PathParameter("id", "agent id")).
		Param(ws.FormParameter(fileFormField, "one or more document files to add to the agent's RAG collection (repeat the field per file)").DataType("file").Required(true).AllowMultiple(true)).
		Doc("Upload one or more documents to the agent's RAG configuration; each is chunked and embedded asynchronously and retrieved at inference time").
		Returns(http.StatusAccepted, "Accepted for indexing", uploadRAGDocumentResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Agent not found", errorResponse{}))

	return ws
}

// validate checks the model is known and that each referenced knowledge base
// exists, the latter by asking the KB service (the owner of that domain)
// rather than reading its storage directly.
func (a *AgentAPI) validate(req *restful.Request, logger *logging.Logger, body agentRequest) (string, bool) {
	if body.Model == "" {
		logger.Warn("agent validation failed: model is required")
		return "model is required", false
	}
	if _, ok := models.Resolve(body.Model); !ok {
		logger.Warn("agent validation failed: unknown model", "model", body.Model)
		return fmt.Sprintf("unknown model %q; see GET /v1/models", body.Model), false
	}
	return a.validateKBs(req, logger, body.KnowledgeBaseIDs)
}

// validateKBs checks that every referenced knowledge base exists, asking the
// KB service (the owner of that domain).
func (a *AgentAPI) validateKBs(req *restful.Request, logger *logging.Logger, kbIDs []string) (string, bool) {
	for _, kbID := range kbIDs {
		_, found, err := a.kbs.Get(req.Request.Context(), kbID)
		if err != nil {
			logger.Error("failed to validate knowledge base", "kb_id", kbID, "err", err)
			return "failed to validate knowledge bases", false
		}
		if !found {
			logger.Warn("agent validation failed: knowledge base not found", "kb_id", kbID)
			return fmt.Sprintf("knowledge base %q not found", kbID), false
		}
	}
	return "", true
}

func (a *AgentAPI) create(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("create agent requested")
	body, ok := decode(req, resp, logger)
	if !ok {
		return
	}
	logger.Info("create request decoded", "model", body.Model, "knowledge_base_ids", body.KnowledgeBaseIDs, "system_prompt_chars", len(body.SystemPrompt))
	if msg, valid := a.validate(req, logger, body); !valid {
		writeError(req, resp, http.StatusBadRequest, msg)
		return
	}
	logger.Info("agent validated")
	now := time.Now().UTC()
	agent := agents.Agent{
		ID:               uuid.NewString(),
		UserID:           userID,
		Model:            body.Model,
		SystemPrompt:     body.SystemPrompt,
		KnowledgeBaseIDs: normalizeIDs(body.KnowledgeBaseIDs),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := a.agents.Create(req.Request.Context(), agent); err != nil {
		logger.Error("failed to create agent", "agent_id", agent.ID, "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to create agent")
		return
	}
	logger.Info("agent created", "agent_id", agent.ID, "model", agent.Model, "knowledge_base_ids", agent.KnowledgeBaseIDs)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, toAgentResponse(agent))
}

func (a *AgentAPI) list(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("list agents requested")
	records, err := a.agents.List(req.Request.Context(), userID)
	if err != nil {
		logger.Error("failed to list agents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list agents")
		return
	}
	logger.Info("agents listed", "count", len(records))
	out := make([]agentResponse, 0, len(records))
	for _, ag := range records {
		out = append(out, toAgentResponse(ag))
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, listAgentsResponse{Agents: out})
}

func (a *AgentAPI) get(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("get agent requested")
	agent, found, err := a.agents.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to get agent", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to get agent")
		return
	}
	if !found {
		logger.Warn("agent not found")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent fetched", "model", agent.Model, "knowledge_base_ids", agent.KnowledgeBaseIDs)
	requesthelper.WriteJSON(req, resp, http.StatusOK, toAgentResponse(agent))
}

func (a *AgentAPI) update(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("update agent requested")
	existing, found, err := a.agents.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to look up agent for update", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to update agent")
		return
	}
	if !found {
		logger.Warn("agent not found for update")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent loaded", "model", existing.Model, "knowledge_base_ids", existing.KnowledgeBaseIDs)
	var body agentUpdateRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid update request body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("update request decoded",
		"model_provided", body.Model != nil,
		"system_prompt_provided", body.SystemPrompt != nil,
		"knowledge_base_ids_provided", body.KnowledgeBaseIDs != nil,
	)

	// Partial update: only provided fields change, and only provided fields
	// are validated — untouched KB references are not re-checked.
	if body.Model != nil {
		if _, ok := models.Resolve(*body.Model); !ok {
			logger.Warn("agent update failed: unknown model", "model", *body.Model)
			writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("unknown model %q; see GET /v1/models", *body.Model))
			return
		}
		existing.Model = *body.Model
	}
	if body.SystemPrompt != nil {
		existing.SystemPrompt = *body.SystemPrompt
	}
	if body.KnowledgeBaseIDs != nil {
		if msg, valid := a.validateKBs(req, logger, *body.KnowledgeBaseIDs); !valid {
			writeError(req, resp, http.StatusBadRequest, msg)
			return
		}
		existing.KnowledgeBaseIDs = normalizeIDs(*body.KnowledgeBaseIDs)
	}
	logger.Info("agent changes applied", "model", existing.Model, "knowledge_base_ids", existing.KnowledgeBaseIDs, "system_prompt_chars", len(existing.SystemPrompt))
	existing.UpdatedAt = time.Now().UTC()
	if err := a.agents.Update(req.Request.Context(), existing); err != nil {
		logger.Error("failed to update agent", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to update agent")
		return
	}
	logger.Info("agent updated", "model", existing.Model, "knowledge_base_ids", existing.KnowledgeBaseIDs)
	requesthelper.WriteJSON(req, resp, http.StatusOK, toAgentResponse(existing))
}

func (a *AgentAPI) delete(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("delete agent requested")
	_, found, err := a.agents.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to look up agent for deletion", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete agent")
		return
	}
	if !found {
		logger.Warn("agent not found for deletion")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	if err := a.agents.Delete(req.Request.Context(), id); err != nil {
		logger.Error("failed to delete agent", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete agent")
		return
	}
	logger.Info("agent record deleted")
	// Cascade: remove the agent's RAG collection.
	if err := a.rags.DeleteByAgent(req.Request.Context(), id); err != nil {
		logger.Error("failed to delete agent rag collection", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete agent RAG documents")
		return
	}
	logger.Info("agent deleted")
	resp.WriteHeader(http.StatusNoContent)
}

// uploadRAGDocument accepts a multipart file upload for the agent's RAG
// configuration and enqueues it for asynchronous chunking and embedding.
func (a *AgentAPI) uploadRAGDocument(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("rag upload requested")

	agent, found, err := a.agents.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to look up agent for rag upload", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up agent")
		return
	}
	if !found {
		logger.Warn("agent not found for rag upload")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent loaded")

	// All files are read and validated up front, so a bad file rejects the
	// whole request before anything is enqueued.
	files, status, msg := requesthelper.ReadUploadedFiles(req, resp, fileFormField, maxUploadBytes)
	if status != 0 {
		logger.Warn("invalid rag upload", "err", msg)
		writeError(req, resp, status, msg)
		return
	}
	logger.Info("upload files read", "files", len(files))

	queued := make([]queuedDocument, 0, len(files))
	for _, f := range files {
		docID := uuid.NewString()
		message := queue.DocumentMessage{
			Type:       queue.DocumentTypeAgentRAG,
			UserID:     agent.UserID,
			AgentID:    agent.ID,
			DocumentID: docID,
			Filename:   f.Filename,
			// Content carries the raw document bytes base64-encoded; the indexer
			// decodes them and hands the bytes to Tika for text extraction.
			Content: base64.StdEncoding.EncodeToString(f.Content),
		}
		if err := a.producer.Publish(req.Request.Context(), message); err != nil {
			logger.Error("failed to enqueue rag document for indexing",
				"document_id", docID, "file_name", f.Filename, "queued", len(queued), "total", len(files), "err", err)
			writeError(req, resp, http.StatusBadGateway,
				fmt.Sprintf("failed to enqueue %q for indexing (%d of %d files were queued)", f.Filename, len(queued), len(files)))
			return
		}
		logger.Info("rag document queued for indexing",
			"document_id", docID, "file_name", f.Filename, "size_bytes", len(f.Content))
		queued = append(queued, queuedDocument{DocumentID: docID, Filename: f.Filename, Status: "queued"})
	}
	logger.Info("rag upload accepted", "documents", len(queued))
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, uploadRAGDocumentResponse{
		AgentID:   agent.ID,
		Documents: queued,
	})
}

func decode(req *restful.Request, resp *restful.Response, logger *logging.Logger) (agentRequest, bool) {
	var body agentRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid request body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return agentRequest{}, false
	}
	return body, true
}

func normalizeIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func toAgentResponse(a agents.Agent) agentResponse {
	return agentResponse{
		ID:               a.ID,
		UserID:           a.UserID,
		Model:            a.Model,
		SystemPrompt:     a.SystemPrompt,
		KnowledgeBaseIDs: normalizeIDs(a.KnowledgeBaseIDs),
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}

func writeError(req *restful.Request, resp *restful.Response, status int, msg string) {
	requesthelper.WriteError(req, resp, status, msg)
}
