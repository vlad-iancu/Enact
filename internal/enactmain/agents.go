package enactmain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// agentUploadMaxBytes caps RAG document uploads forwarded to the agent
// service, matching its own limit.
const agentUploadMaxBytes = 50 << 20

// agentListItem is an agent plus what the caller may do with it. The agent's
// own fields are embedded, so the JSON is the agent object with two extra
// keys rather than a wrapper the UI has to unpack.
type agentListItem struct {
	agents.Agent
	resourceFlags
}

type listAgentsResponse struct {
	Agents []agentListItem `json:"agents"`
}

type listAgentRAGDocumentsResponse struct {
	AgentID   string               `json:"agent_id"`
	Documents []agents.RAGDocument `json:"documents"`
}

type queuedDeletionResponse struct {
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
}

// agentsWebService returns the session-guarded agent management routes: the
// UI's window onto the agent service, proxied caller-for-caller so the
// session user's data scoping applies downstream.
func (a *MainAPI) agentsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/agents").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.GET("").
		To(a.listAgents).
		Doc("List the logged-in user's agents").
		Returns(http.StatusOK, "OK", listAgentsResponse{}))

	ws.Route(ws.GET("/{id}/required-identities").
		To(a.agentRequiredIdentities).
		Param(ws.PathParameter("id", "agent id")).
		Doc("What the logged-in user must connect before this agent's tools can run, and whether they already have").
		Returns(http.StatusOK, "OK", agentRequirementsResponse{}).
		Returns(http.StatusNotFound, "No such agent", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.POST("").
		To(a.createAgent).
		Consumes(restful.MIME_JSON).
		Reads(agents.CreateAgentRequest{}).
		Doc("Create an agent").
		Returns(http.StatusCreated, "Created", agents.Agent{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.getAgent).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Get an agent").
		Returns(http.StatusOK, "OK", agents.Agent{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateAgent).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Partially update an agent (absent fields keep their value)").
		Returns(http.StatusOK, "OK", agents.Agent{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteAgent).
		Param(ws.PathParameter("id", "agent id")).
		Doc("Delete an agent and its RAG collection").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}/rag/documents").
		To(a.listAgentRAGDocuments).
		Param(ws.PathParameter("id", "agent id")).
		Doc("List the documents of the agent's RAG configuration").
		Returns(http.StatusOK, "OK", listAgentRAGDocumentsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/rag/documents").
		To(a.uploadAgentRAGDocuments).
		Consumes("multipart/form-data").
		Param(ws.PathParameter("id", "agent id")).
		Param(ws.FormParameter("file", "one or more document files (repeat the field per file)").DataType("file").Required(true).AllowMultiple(true)).
		Doc("Upload documents to the agent's RAG configuration; indexing is asynchronous").
		Returns(http.StatusAccepted, "Accepted for indexing", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/rag/documents/{docId}").
		To(a.deleteAgentRAGDocument).
		Param(ws.PathParameter("id", "agent id")).
		Param(ws.PathParameter("docId", "document id")).
		Doc("Queue the removal of one RAG document; deletion is asynchronous").
		Returns(http.StatusAccepted, "Accepted for deletion", queuedDeletionResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

// relayAgentErr maps a downstream client error onto the right HTTP reply:
// validation messages pass through as 400s, everything else is a 502.
func relayAgentErr(req *restful.Request, resp *restful.Response, err error, action string) {
	var forbidden *requesthelper.ForbiddenError
	if errors.As(err, &forbidden) {
		// A downstream refusal, relayed as one. Flattening it into the 502
		// below would tell a user their platform is broken when in fact they
		// need a role.
		requesthelper.WriteError(req, resp, http.StatusForbidden, forbidden.Message)
		return
	}
	var badReq *requesthelper.BadRequestError
	if errors.As(err, &badReq) {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, badReq.Message)
		return
	}
	requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to "+action)
}

func (a *MainAPI) listAgents(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list agents requested")
	list, err := a.agents.List(req.Request.Context())
	if err != nil {
		logger.Error("failed to list agents", "err", err)
		relayAgentErr(req, resp, err, "list agents")
		return
	}
	// One rules lookup for the whole page; the flags are then decided
	// locally with the same matcher the services enforce with, so the UI
	// cannot disagree with what a later edit or delete would answer.
	//
	// A failure here leaves both flags false rather than failing the
	// listing: the agents are the answer, and a missing edit button is a
	// better outcome than no list at all.
	effective := a.effectiveFor(req, logger, sess.UserID)
	out := make([]agentListItem, 0, len(list))
	usable := 0
	for _, agent := range list {
		item := agentListItem{Agent: agent, resourceFlags: flagsFor(effective, rbac.ResourceAgent, agent.ID)}
		if item.Usable {
			usable++
		}
		out = append(out, item)
	}
	logger.Info("agents listed", "count", len(out), "usable", usable, "owner", effective.Owner)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listAgentsResponse{Agents: out})
}

func (a *MainAPI) createAgent(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create agent requested")

	var body agents.CreateAgentRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create agent body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("create request decoded", "model", body.Model, "knowledge_base_ids", body.KnowledgeBaseIDs, "system_prompt_chars", len(body.SystemPrompt))

	created, err := a.agents.Create(req.Request.Context(), body)
	if err != nil {
		logger.Warn("agent creation failed", "err", err)
		relayAgentErr(req, resp, err, "create agent")
		return
	}
	logger.Info("agent created", "agent_id", created.ID)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) getAgent(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id)
	logger.Info("get agent requested")

	agent, found, err := a.agents.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to get agent", "err", err)
		relayAgentErr(req, resp, err, "get agent")
		return
	}
	if !found {
		logger.Warn("agent not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	item := agentListItem{Agent: agent, resourceFlags: flagsFor(effective, rbac.ResourceAgent, agent.ID)}
	logger.Info("agent fetched", "name", agent.Name, "model", agent.Model, "usable", item.Usable)
	requesthelper.WriteJSON(req, resp, http.StatusOK, item)
}

func (a *MainAPI) updateAgent(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id)
	logger.Info("update agent requested")

	// Raw passthrough preserves the agent service's partial-update
	// semantics (absent field = keep) end to end.
	rawBody, err := io.ReadAll(req.Request.Body)
	if err != nil {
		logger.Warn("failed to read update body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
		return
	}
	logger.Info("update body read", "bytes", len(rawBody))

	updated, found, err := a.agents.Update(req.Request.Context(), id, rawBody)
	if err != nil {
		logger.Warn("agent update failed", "err", err)
		relayAgentErr(req, resp, err, "update agent")
		return
	}
	if !found {
		logger.Warn("agent not found for update")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent updated")
	requesthelper.WriteJSON(req, resp, http.StatusOK, updated)
}

func (a *MainAPI) deleteAgent(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id)
	logger.Info("delete agent requested")

	found, err := a.agents.Delete(req.Request.Context(), id)
	if err != nil {
		logger.Error("agent deletion failed", "err", err)
		relayAgentErr(req, resp, err, "delete agent")
		return
	}
	if !found {
		logger.Warn("agent not found for deletion")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("agent deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) listAgentRAGDocuments(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id)
	logger.Info("list rag documents requested")

	docs, found, err := a.agents.ListRAGDocuments(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to list rag documents", "err", err)
		relayAgentErr(req, resp, err, "list RAG documents")
		return
	}
	if !found {
		logger.Warn("agent not found for rag listing")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("rag documents listed", "documents", len(docs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listAgentRAGDocumentsResponse{AgentID: id, Documents: docs})
}

func (a *MainAPI) uploadAgentRAGDocuments(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id)
	logger.Info("rag upload requested")

	files, status, msg := requesthelper.ReadUploadedFiles(req, resp, "file", agentUploadMaxBytes)
	if status != 0 {
		logger.Warn("invalid rag upload", "err", msg)
		requesthelper.WriteError(req, resp, status, msg)
		return
	}
	logger.Info("upload files read", "files", len(files))

	body, found, err := a.agents.UploadRAGDocuments(req.Request.Context(), id, files)
	if err != nil {
		logger.Error("rag upload forward failed", "err", err)
		relayAgentErr(req, resp, err, "upload RAG documents")
		return
	}
	if !found {
		logger.Warn("agent not found for rag upload")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("rag upload accepted", "files", len(files))
	writeRawJSON(resp, http.StatusAccepted, body)
}

func (a *MainAPI) deleteAgentRAGDocument(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	docID := req.PathParameter("docId")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", id, "document_id", docID)
	logger.Info("rag document deletion requested")

	found, err := a.agents.DeleteRAGDocument(req.Request.Context(), id, docID)
	if err != nil {
		logger.Error("rag document deletion failed", "err", err)
		relayAgentErr(req, resp, err, "delete RAG document")
		return
	}
	if !found {
		logger.Warn("agent not found for rag deletion")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	logger.Info("rag document deletion queued")
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, queuedDeletionResponse{DocumentID: docID, Status: "queued"})
}

// writeRawJSON relays a downstream JSON body verbatim (it already carries
// its own meta block from the downstream service).
func writeRawJSON(resp *restful.Response, status int, body json.RawMessage) {
	resp.Header().Set("Content-Type", restful.MIME_JSON)
	resp.WriteHeader(status)
	_, _ = resp.Write(body)
}
