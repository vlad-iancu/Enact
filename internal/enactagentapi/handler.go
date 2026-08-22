package enactagentapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/agents"
	"enact/internal/identity"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/models"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// maxUploadBytes caps the size of a RAG document upload. Requests whose body
// exceeds this are rejected before being read into memory.
const maxUploadBytes = 50 << 20 // 50 MiB

// fileFormField is the multipart form field carrying the document file.
const fileFormField = "file"

// maxOutputSchemaBytes caps an agent's output schema. It is stored on the
// record and sent to Bedrock on every turn of every inference, so an
// unbounded one is paid for repeatedly. Real schemas are a few hundred bytes;
// this is far above anything legitimate and well below anything harmful.
const maxOutputSchemaBytes = 64 << 10 // 64 KiB

// AgentAPI exposes agent CRUD and RAG document upload. It uses the agent
// repository for the CRUD, the KB service client to validate the knowledge
// bases an agent references (a live call to enact-kb-api), the RAG repository
// to cascade-delete an agent's RAG collection, and the queue producer to hand
// RAG documents to the indexer.
type AgentAPI struct {
	agents   *agents.Repository
	rags     *agents.RAGRepository
	kbs      *kb.Client
	tools    *tools.Client
	producer *queue.Producer
	// rbac records who owns what; enforcer answers whether the caller may
	// act. Both are needed: creating a resource grants ownership of it, and
	// every other path checks.
	rbac     *rbac.Client
	enforcer *rbac.Enforcer
	logger   *logging.Logger
}

func newAgentAPI(agentRepo *agents.Repository, rags *agents.RAGRepository, kbs *kb.Client, toolsClient *tools.Client, producer *queue.Producer, rbacClient *rbac.Client, enforcer *rbac.Enforcer, logger *logging.Logger) *AgentAPI {
	return &AgentAPI{
		agents: agentRepo, rags: rags, kbs: kbs, tools: toolsClient, producer: producer,
		rbac: rbacClient, enforcer: enforcer, logger: logger,
	}
}

type agentRequest struct {
	Name             string   `json:"name"`
	Model            string   `json:"model"`
	SystemPrompt     string   `json:"system_prompt"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	// Tools names registered MCP servers whose tools the agent may call.
	Tools []string `json:"tools"`
	// OutputSchema is a JSON Schema constraining the assistant's reply.
	OutputSchema json.RawMessage `json:"output_schema"`
}

// agentUpdateRequest is the partial-update body: pointer fields distinguish
// "absent — leave unchanged" (nil) from "provided — set to this value",
// including explicit clears ("" for the prompt, [] for the KB list). A JSON
// null is indistinguishable from an absent field and also means "unchanged".
type agentUpdateRequest struct {
	Name             *string   `json:"name"`
	Model            *string   `json:"model"`
	SystemPrompt     *string   `json:"system_prompt"`
	KnowledgeBaseIDs *[]string `json:"knowledge_base_ids"`
	Tools            *[]string `json:"tools"`
	// OutputSchema follows the same rule, with one wrinkle: null means
	// "unchanged" here as everywhere else, so it cannot also mean "remove".
	// The empty object {} is the clear — a schema that constrains nothing is
	// indistinguishable from having none, so the two are one thing.
	OutputSchema *json.RawMessage `json:"output_schema"`
}

type agentResponse struct {
	ID               string          `json:"id"`
	UserID           string          `json:"user_id"`
	Name             string          `json:"name"`
	Model            string          `json:"model"`
	SystemPrompt     string          `json:"system_prompt"`
	KnowledgeBaseIDs []string        `json:"knowledge_base_ids"`
	Tools            []string        `json:"tools"`
	OutputSchema     json.RawMessage `json:"output_schema,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
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

// ragDocumentResponse is one distinct document of an agent's RAG collection.
type ragDocumentResponse struct {
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Chunks     int    `json:"chunks"`
}

type listRAGDocumentsResponse struct {
	AgentID   string                `json:"agent_id"`
	Documents []ragDocumentResponse `json:"documents"`
}

// deleteRAGDocumentResponse acknowledges an asynchronous document deletion.
type deleteRAGDocumentResponse struct {
	AgentID    string `json:"agent_id"`
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// organization resolves the caller's organization for a scoped lookup,
// writing the refusal itself when they have none. Every read passes through
// it, so no path can accidentally fetch across the boundary.
func (a *AgentAPI) organization(req *restful.Request, resp *restful.Response, notFound string) (string, bool) {
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDenied(req, resp, err, notFound)
		return "", false
	}
	return organizationID, true
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
		Doc("Partially update an agent: only provided fields change, absent fields keep their value (clear with \"\" / [], and {} for output_schema)").
		Returns(http.StatusOK, "OK", agentResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.delete).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Delete an agent and its RAG collection").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}/rag/documents").
		To(a.listRAGDocuments).
		Param(ws.PathParameter("id", "agent id")).
		Doc("List the distinct documents of the agent's RAG collection (document id, filename, chunk count)").
		Returns(http.StatusOK, "OK", listRAGDocumentsResponse{}).
		Returns(http.StatusNotFound, "Agent not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/rag/documents/{docId}").
		To(a.deleteRAGDocument).
		Param(ws.PathParameter("id", "agent id")).
		Param(ws.PathParameter("docId", "document id")).
		Doc("Queue the removal of one RAG document's chunks; deletion is asynchronous (a nonexistent document id is still accepted)").
		Returns(http.StatusAccepted, "Accepted for deletion", deleteRAGDocumentResponse{}).
		Returns(http.StatusNotFound, "Agent not found", errorResponse{}))

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
	if strings.TrimSpace(body.Name) == "" {
		logger.Warn("agent validation failed: name is required")
		return "name is required", false
	}
	if msg, ok := validateOutputSchema(logger, body.OutputSchema); !ok {
		return msg, false
	}
	if msg, ok := a.validateKBs(req, logger, body.KnowledgeBaseIDs); !ok {
		return msg, false
	}
	return a.validateTools(req, logger, body.Tools)
}

// validateOutputSchema checks an agent's structured-output schema.
//
// It deliberately checks shape and size only, not JSON Schema semantics.
// Bedrock is the authority on which keywords it honours and the models
// disagree with each other about the fringes; re-implementing that judgement
// here would mean rejecting schemas that work, which is worse than passing
// through one that does not. What is checked is what we would otherwise only
// discover mid-inference, far from the person who typed it.
//
// The empty object is accepted and normalized away by the caller: it is the
// documented way to clear the field.
func validateOutputSchema(logger *logging.Logger, raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if len(raw) > maxOutputSchemaBytes {
		logger.Warn("agent validation failed: output schema too large", "bytes", len(raw), "limit", maxOutputSchemaBytes)
		return fmt.Sprintf("output_schema is %d bytes; the limit is %d", len(raw), maxOutputSchemaBytes), false
	}
	// The body decoder has already proven this is well-formed JSON, so the
	// only thing left to establish is that it is an object. A JSON Schema
	// document always is — including one describing an array, which is
	// {"type":"array",…} and therefore still an object here.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		logger.Warn("agent validation failed: output schema is not a json object", "err", err)
		return "output_schema must be a JSON Schema object, for example {\"type\":\"object\",\"properties\":{…}}", false
	}
	return "", true
}

// isEmptyJSONObject reports whether raw is {} — the sentinel that clears an
// output schema, since a null would be indistinguishable from an absent field.
func isEmptyJSONObject(raw json.RawMessage) bool {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return len(doc) == 0
}

// normalizeOutputSchema maps "no schema" and "the empty schema" onto one
// representation — absent — so a cleared agent and a never-configured one are
// the same record, and inference has a single case to check.
func normalizeOutputSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || isEmptyJSONObject(raw) {
		return nil
	}
	return raw
}

// validateTools checks that every referenced MCP server is registered,
// asking the tool registry (the owner of that domain).
func (a *AgentAPI) validateTools(req *restful.Request, logger *logging.Logger, serverIDs []string) (string, bool) {
	if len(serverIDs) == 0 {
		return "", true
	}
	servers, err := a.tools.List(req.Request.Context(), serverIDs, "")
	if err != nil {
		logger.Error("failed to validate mcp servers", "server_ids", serverIDs, "err", err)
		return "failed to validate MCP servers", false
	}
	known := make(map[string]bool, len(servers))
	for _, s := range servers {
		known[s.ID] = true
	}
	for _, id := range serverIDs {
		if !known[id] {
			logger.Warn("agent validation failed: mcp server not found", "server_id", id)
			return fmt.Sprintf("MCP server %q not found", id), false
		}
	}
	return "", true
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
	if err := a.enforcer.Require(req.Request.Context(), rbac.Permission(rbac.ResourceAgent, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("create agent denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	body, ok := decode(req, resp, logger)
	if !ok {
		return
	}
	logger.Info("create request decoded", "model", body.Model, "knowledge_base_ids", body.KnowledgeBaseIDs,
		"system_prompt_chars", len(body.SystemPrompt), "output_schema_bytes", len(body.OutputSchema))
	if msg, valid := a.validate(req, logger, body); !valid {
		writeError(req, resp, http.StatusBadRequest, msg)
		return
	}
	logger.Info("agent validated")
	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	now := time.Now().UTC()
	agent := agents.Agent{
		ID:               uuid.NewString(),
		UserID:           userID,
		OrganizationID:   organizationID,
		Name:             strings.TrimSpace(body.Name),
		Model:            body.Model,
		SystemPrompt:     body.SystemPrompt,
		KnowledgeBaseIDs: normalizeIDs(body.KnowledgeBaseIDs),
		Tools:            normalizeIDs(body.Tools),
		OutputSchema:     normalizeOutputSchema(body.OutputSchema),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := a.agents.Create(req.Request.Context(), agent); err != nil {
		logger.Error("failed to create agent", "agent_id", agent.ID, "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to create agent")
		return
	}
	// The creator owns it. Recorded before replying, so a client that reads
	// the agent back immediately can: a grant landing after the response
	// would be a race the caller loses.
	if err := a.rbac.Grant(req.Request.Context(), rbac.GrantRequest{
		UserID:     userID,
		Resource:   rbac.ResourceAgent,
		ResourceID: agent.ID,
	}); err != nil {
		logger.Error("failed to record ownership", "agent_id", agent.ID, "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to record ownership of the agent")
		return
	}
	// The caller's cached rules predate this grant; without dropping them
	// they would be refused the agent they just created until the cache TTL
	// expired.
	a.enforcer.Forget(userID)
	logger.Info("agent created", "agent_id", agent.ID, "name", agent.Name, "model", agent.Model, "knowledge_base_ids", agent.KnowledgeBaseIDs)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, toAgentResponse(agent))
}

func (a *AgentAPI) list(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("list agents requested")
	// Candidates are the organization's; the caller's rules decide which of
	// them they may see. A user with no roles still gets exactly their own,
	// through the hidden ownership role rather than a hard-coded owner
	// filter.
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	effective, err := a.enforcer.CallerEffective(req.Request.Context())
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	records, err := a.agents.List(req.Request.Context(), organizationID)
	if err != nil {
		logger.Error("failed to list agents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list agents")
		return
	}
	out := make([]agentResponse, 0, len(records))
	for _, ag := range records {
		if !effective.Allows(rbac.Permission(rbac.ResourceAgent, rbac.ActionView, ag.ID)) {
			continue
		}
		out = append(out, toAgentResponse(ag))
	}
	logger.Info("agents listed", "candidates", len(records), "visible", len(out),
		"organization_id", organizationID)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listAgentsResponse{Agents: out})
}

func (a *AgentAPI) get(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("get agent requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionView, id); err != nil {
		logger.Warn("get agent denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}
	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	agent, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
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
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionEdit, id); err != nil {
		logger.Warn("update agent denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}
	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	existing, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
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
		"name_provided", body.Name != nil,
		"model_provided", body.Model != nil,
		"system_prompt_provided", body.SystemPrompt != nil,
		"knowledge_base_ids_provided", body.KnowledgeBaseIDs != nil,
		"output_schema_provided", body.OutputSchema != nil,
	)

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeError(req, resp, http.StatusBadRequest, "name must not be empty")
			return
		}
		existing.Name = name
	}

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
	if body.Tools != nil {
		if msg, valid := a.validateTools(req, logger, *body.Tools); !valid {
			writeError(req, resp, http.StatusBadRequest, msg)
			return
		}
		existing.Tools = normalizeIDs(*body.Tools)
	}
	if body.OutputSchema != nil {
		if msg, valid := validateOutputSchema(logger, *body.OutputSchema); !valid {
			writeError(req, resp, http.StatusBadRequest, msg)
			return
		}
		existing.OutputSchema = normalizeOutputSchema(*body.OutputSchema)
	}
	logger.Info("agent changes applied", "model", existing.Model, "knowledge_base_ids", existing.KnowledgeBaseIDs,
		"system_prompt_chars", len(existing.SystemPrompt), "output_schema_bytes", len(existing.OutputSchema))
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
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionDelete, id); err != nil {
		logger.Warn("delete agent denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}
	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	_, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
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
	// A RAG document belongs to its agent, so editing it is editing the agent.
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionEdit, id); err != nil {
		logger.Warn("rag upload denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	agent, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
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

// listRAGDocuments lists the distinct documents of an agent's RAG
// collection, aggregated from its chunk index.
func (a *AgentAPI) listRAGDocuments(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id)
	logger.Info("rag document listing requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionView, id); err != nil {
		logger.Warn("rag listing denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	agent, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up agent for rag listing", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up agent")
		return
	}
	if !found {
		logger.Warn("agent not found for rag listing")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent loaded")

	docs, err := a.rags.ListDocuments(req.Request.Context(), agent.UserID, agent.ID)
	if err != nil {
		logger.Error("failed to list rag documents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list RAG documents")
		return
	}
	out := make([]ragDocumentResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, ragDocumentResponse{DocumentID: d.DocumentID, Filename: d.Filename, Chunks: d.Chunks})
	}
	logger.Info("rag documents listed", "documents", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listRAGDocumentsResponse{AgentID: agent.ID, Documents: out})
}

// deleteRAGDocument queues the asynchronous removal of one RAG document's
// chunks; the indexer performs the delete, scoped to this agent.
func (a *AgentAPI) deleteRAGDocument(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	docID := req.PathParameter("docId")
	logger := requesthelper.Logger(req, a.logger).WithFields("agent_id", id, "document_id", docID)
	logger.Info("rag document deletion requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceAgent, rbac.ActionEdit, id); err != nil {
		logger.Warn("rag deletion denied", "err", err)
		rbac.WriteDenied(req, resp, err, "agent not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "agent not found")
	if !ok {
		return
	}
	agent, found, err := a.agents.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up agent for rag deletion", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up agent")
		return
	}
	if !found {
		logger.Warn("agent not found for rag deletion")
		writeError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent loaded")

	message := queue.DocumentMessage{
		Type:       queue.DocumentTypeAgentRAGDelete,
		UserID:     agent.UserID,
		AgentID:    agent.ID,
		DocumentID: docID,
	}
	if err := a.producer.Publish(req.Request.Context(), message); err != nil {
		logger.Error("failed to enqueue rag document deletion", "err", err)
		writeError(req, resp, http.StatusBadGateway, "failed to enqueue document deletion")
		return
	}
	logger.Info("rag document deletion queued")
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, deleteRAGDocumentResponse{
		AgentID:    agent.ID,
		DocumentID: docID,
		Status:     "queued",
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
		Name:             a.Name,
		Model:            a.Model,
		SystemPrompt:     a.SystemPrompt,
		KnowledgeBaseIDs: normalizeIDs(a.KnowledgeBaseIDs),
		Tools:            normalizeIDs(a.Tools),
		OutputSchema:     a.OutputSchema,
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}

func writeError(req *restful.Request, resp *restful.Response, status int, msg string) {
	requesthelper.WriteError(req, resp, status, msg)
}
