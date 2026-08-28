package enactmain

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/kb"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// kbUploadMaxBytes caps context-document uploads forwarded to the KB
// service, matching its own limit.
const kbUploadMaxBytes = 50 << 20

// kbListItem is a knowledge base plus what the caller may do with it; see
// resourceFlags. "Usable" for a knowledge base means retrieving from it —
// what an agent does at inference time — as distinct from editing the record.
type kbListItem struct {
	kb.KnowledgeBase
	resourceFlags
}

type listKBsResponse struct {
	KnowledgeBases []kbListItem `json:"knowledge_bases"`
}

// createKBRequest names the knowledge base being created and says what kind
// it is; an absent kind means "context", the behaviour every knowledge base
// had before kinds existed.
type createKBRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// ChunkSize and ChunkOverlap tune a retrieval KB's document splitting, in
	// runes, and can only be set here — creation. Absent takes the platform
	// default; the KB service validates and refuses them on a context KB.
	ChunkSize    *int `json:"chunk_size,omitempty"`
	ChunkOverlap *int `json:"chunk_overlap,omitempty"`
}

// queuedDeletionResponse acknowledges an asynchronous document deletion.
type queuedDeletionResponse struct {
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
}

// kbWebService returns the session-guarded knowledge-base routes: the UI's
// window onto the (otherwise internal) KB service.
func (a *MainAPI) kbWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/knowledge-bases").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireCaller)

	ws.Route(ws.GET("").
		To(a.listKBs).
		Doc("List the logged-in user's knowledge bases").
		Returns(http.StatusOK, "OK", listKBsResponse{}))

	ws.Route(ws.POST("").
		To(a.createKB).
		Consumes(restful.MIME_JSON).
		Reads(createKBRequest{}).
		Doc("Create a knowledge base with a friendly name").
		Returns(http.StatusCreated, "Created", kb.KnowledgeBase{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateKB).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Partially update a knowledge base's own fields (currently: name)").
		Returns(http.StatusOK, "OK", kb.KnowledgeBase{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.getKB).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Get a knowledge base and the metadata of its documents").
		Returns(http.StatusOK, "OK", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteKB).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Delete a knowledge base and all of its documents").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/documents").
		To(a.uploadKBDocuments).
		Consumes("multipart/form-data").
		Param(ws.PathParameter("id", "knowledge base id")).
		Param(ws.FormParameter("file", "one or more document files (repeat the field per file)").DataType("file").Required(true).AllowMultiple(true)).
		Doc("Upload context documents; indexing is asynchronous").
		Returns(http.StatusAccepted, "Accepted for indexing", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/documents/{docId}").
		To(a.deleteKBDocument).
		Param(ws.PathParameter("id", "knowledge base id")).
		Param(ws.PathParameter("docId", "document id")).
		Doc("Queue the removal of one context document; deletion is asynchronous").
		Returns(http.StatusAccepted, "Accepted for deletion", queuedDeletionResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

func (a *MainAPI) listKBs(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list knowledge bases requested")
	list, err := a.kb.List(req.Request.Context())
	if err != nil {
		logger.Error("failed to list knowledge bases", "err", err)
		relayAgentErr(req, resp, err, "list knowledge bases")
		return
	}
	logger.Info("knowledge bases listed", "count", len(list))
	effective := a.effectiveFor(req, logger, sess.UserID)
	out := make([]kbListItem, 0, len(list))
	for _, record := range list {
		out = append(out, kbListItem{
			KnowledgeBase: record,
			resourceFlags: flagsFor(effective, rbac.ResourceKB, record.ID),
		})
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, listKBsResponse{KnowledgeBases: out})
}

func (a *MainAPI) createKB(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create knowledge base requested")

	var body createKBRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("create request decoded", "name", body.Name, "kind", body.Kind,
		"chunk_size", body.ChunkSize, "chunk_overlap", body.ChunkOverlap)

	record, err := a.kb.Create(req.Request.Context(), kb.CreateRequest{
		Name:         body.Name,
		Kind:         body.Kind,
		ChunkSize:    body.ChunkSize,
		ChunkOverlap: body.ChunkOverlap,
	})
	if err != nil {
		logger.Warn("knowledge base creation failed", "err", err)
		relayAgentErr(req, resp, err, "create knowledge base")
		return
	}
	logger.Info("knowledge base created", "kb_id", record.ID, "name", record.Name)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, record)
}

// updateKB relays a partial update of the knowledge base's own fields; the
// raw body passes through so the KB service's partial semantics apply.
func (a *MainAPI) updateKB(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "kb_id", id)
	logger.Info("update knowledge base requested")

	rawBody, err := io.ReadAll(req.Request.Body)
	if err != nil {
		logger.Warn("failed to read update body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
		return
	}
	logger.Info("update body read", "bytes", len(rawBody))

	updated, found, err := a.kb.Update(req.Request.Context(), id, rawBody)
	if err != nil {
		logger.Warn("knowledge base update failed", "err", err)
		relayAgentErr(req, resp, err, "update knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for update")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base updated", "name", updated.Name)
	requesthelper.WriteJSON(req, resp, http.StatusOK, updated)
}

func (a *MainAPI) getKB(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "kb_id", id)
	logger.Info("get knowledge base requested")

	body, found, err := a.kb.GetDetail(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to get knowledge base", "err", err)
		relayAgentErr(req, resp, err, "get knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	body = withFlags(body, flagsFor(effective, rbac.ResourceKB, id))
	logger.Info("knowledge base fetched", "bytes", len(body))
	writeRawJSON(resp, http.StatusOK, body)
}

func (a *MainAPI) deleteKB(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "kb_id", id)
	logger.Info("delete knowledge base requested")

	found, err := a.kb.Delete(req.Request.Context(), id)
	if err != nil {
		logger.Error("knowledge base deletion failed", "err", err)
		relayAgentErr(req, resp, err, "delete knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for deletion")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) uploadKBDocuments(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "kb_id", id)
	logger.Info("document upload requested")

	files, status, msg := requesthelper.ReadUploadedFiles(req, resp, "file", kbUploadMaxBytes)
	if status != 0 {
		logger.Warn("invalid document upload", "err", msg)
		requesthelper.WriteError(req, resp, status, msg)
		return
	}
	logger.Info("upload files read", "files", len(files))

	body, found, err := a.kb.UploadDocuments(req.Request.Context(), id, files)
	if err != nil {
		logger.Error("document upload forward failed", "err", err)
		relayAgentErr(req, resp, err, "upload documents")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for upload")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("document upload accepted", "files", len(files))
	writeRawJSON(resp, http.StatusAccepted, body)
}

func (a *MainAPI) deleteKBDocument(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	docID := req.PathParameter("docId")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "kb_id", id, "document_id", docID)
	logger.Info("document deletion requested")

	found, err := a.kb.DeleteDocument(req.Request.Context(), id, docID)
	if err != nil {
		logger.Error("document deletion failed", "err", err)
		relayAgentErr(req, resp, err, "delete document")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for deletion")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("document deletion queued")
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, queuedDeletionResponse{DocumentID: docID, Status: "queued"})
}
