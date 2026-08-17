package enactkbapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/identity"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// maxUploadBytes caps the size of an uploaded document. Requests whose body
// exceeds this are rejected before being read into memory.
const maxUploadBytes = 50 << 20 // 50 MiB

// fileFormField is the multipart form field carrying the document file.
const fileFormField = "file"

// KBAPI exposes knowledge-base management and document upload. It uses both
// knowledge-base domain repositories: KB metadata for the CRUD and documents
// so a KB delete can cascade to the documents stored under it.
type KBAPI struct {
	kbs       *kb.Repository
	documents *kb.DocumentRepository
	producer  *queue.Producer
	rbac      *rbac.Client
	enforcer  *rbac.Enforcer
	logger    *logging.Logger
}

func newKBAPI(kbs *kb.Repository, documents *kb.DocumentRepository, producer *queue.Producer, rbacClient *rbac.Client, enforcer *rbac.Enforcer, logger *logging.Logger) *KBAPI {
	return &KBAPI{kbs: kbs, documents: documents, producer: producer, rbac: rbacClient, enforcer: enforcer, logger: logger}
}

// createKBRequest names the knowledge base being created.
type createKBRequest struct {
	Name string `json:"name"`
}

// updateKBRequest is the partial-update body. It deliberately covers only
// the KB's own fields, never its documents (those have their own endpoints);
// pointer fields distinguish "absent — keep" from "provided — set".
type updateKBRequest struct {
	Name *string `json:"name"`
}

type knowledgeBaseResponse struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type listKBResponse struct {
	KnowledgeBases []knowledgeBaseResponse `json:"knowledge_bases"`
}

// kbDetailResponse is the single-KB shape: the KB record plus the metadata of
// every document stored under it (never the extracted text — that can be
// megabytes and is only ever consumed server-side at inference time).
type kbDetailResponse struct {
	knowledgeBaseResponse
	Documents []kbDocumentResponse `json:"documents"`
}

type kbDocumentResponse struct {
	DocumentID string    `json:"document_id"`
	Filename   string    `json:"filename,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// listKBDocumentsResponse carries the full extracted text of every document
// in a knowledge base. It backs the endpoint the inference service calls to
// build model context, so unlike kbDetailResponse it includes the text.
type listKBDocumentsResponse struct {
	KBID      string                      `json:"kb_id"`
	Documents []kbDocumentContentResponse `json:"documents"`
}

type kbDocumentContentResponse struct {
	DocumentID string    `json:"document_id"`
	Filename   string    `json:"filename,omitempty"`
	Text       string    `json:"text"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// uploadDocumentResponse acknowledges a multi-file upload: one entry per
// file received, in upload order.
type uploadDocumentResponse struct {
	KBID      string           `json:"kb_id"`
	Documents []queuedDocument `json:"documents"`
}

type queuedDocument struct {
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Status     string `json:"status"`
}

// deleteDocumentResponse acknowledges an asynchronous document deletion.
type deleteDocumentResponse struct {
	KBID       string `json:"kb_id"`
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// organization resolves the caller's organization for a scoped lookup,
// writing the refusal itself when they have none. Every read passes through
// it, so no path can accidentally fetch across the boundary.
func (a *KBAPI) organization(req *restful.Request, resp *restful.Response, notFound string) (string, bool) {
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDenied(req, resp, err, notFound)
		return "", false
	}
	return organizationID, true
}

func (a *KBAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/knowledge-bases").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("").
		To(a.create).
		Consumes(restful.MIME_JSON).
		Reads(createKBRequest{}).
		Doc("Create a knowledge base with a friendly name").
		Returns(http.StatusCreated, "Created", knowledgeBaseResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.update).
		Consumes(restful.MIME_JSON).
		Reads(updateKBRequest{}).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Partially update a knowledge base's own fields (currently: name); documents are managed via their own endpoints").
		Returns(http.StatusOK, "OK", knowledgeBaseResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.list).
		Doc("List the caller's knowledge bases").
		Returns(http.StatusOK, "OK", listKBResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.get).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Get a knowledge base and the documents stored under it").
		Returns(http.StatusOK, "OK", kbDetailResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}/documents").
		To(a.listDocuments).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("List a knowledge base's documents with their extracted text; used by the inference service to build model context").
		Returns(http.StatusOK, "OK", listKBDocumentsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.delete).
		Param(ws.PathParameter("id", "knowledge base id")).
		Doc("Delete a knowledge base and all of its indexed documents").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}/documents/{docId}").
		To(a.deleteDocument).
		Param(ws.PathParameter("id", "knowledge base id")).
		Param(ws.PathParameter("docId", "document id")).
		Doc("Queue the removal of one context document; deletion is asynchronous (a nonexistent document id is still accepted)").
		Returns(http.StatusAccepted, "Accepted for deletion", deleteDocumentResponse{}).
		Returns(http.StatusNotFound, "Knowledge base not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/documents").
		To(a.uploadDocument).
		Consumes("multipart/form-data").
		Param(ws.PathParameter("id", "knowledge base id")).
		Param(ws.FormParameter(fileFormField, "one or more document files to index (repeat the field per file)").DataType("file").Required(true).AllowMultiple(true)).
		Doc("Upload one or more context documents; their text is extracted asynchronously and loaded into the context of agents that reference this knowledge base").
		Returns(http.StatusAccepted, "Accepted for indexing", uploadDocumentResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Knowledge base not found", errorResponse{}))

	return ws
}

func (a *KBAPI) create(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("create knowledge base requested")
	if err := a.enforcer.Require(req.Request.Context(), rbac.Permission(rbac.ResourceKB, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("create knowledge base denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}

	var body createKBRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("create request decoded", "name", body.Name)
	if strings.TrimSpace(body.Name) == "" {
		writeError(req, resp, http.StatusBadRequest, "name is required")
		return
	}

	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	now := time.Now().UTC()
	record := kb.KnowledgeBase{
		ID:             uuid.NewString(),
		OrganizationID: organizationID,
		UserID:         userID,
		Name:           strings.TrimSpace(body.Name),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.kbs.Create(req.Request.Context(), record); err != nil {
		logger.Error("failed to create knowledge base", "kb_id", record.ID, "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to create knowledge base")
		return
	}
	// The creator owns it, recorded before replying so a client that reads
	// the knowledge base back immediately can.
	if err := a.rbac.Grant(req.Request.Context(), rbac.GrantRequest{
		UserID:     userID,
		Resource:   rbac.ResourceKB,
		ResourceID: record.ID,
	}); err != nil {
		logger.Error("failed to record ownership", "kb_id", record.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record ownership of the knowledge base")
		return
	}
	// Drop the caller's cached rules, which predate this grant — see
	// rbac.Enforcer.Forget.
	a.enforcer.Forget(userID)
	logger.Info("knowledge base created", "kb_id", record.ID, "name", record.Name)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, toKBResponse(record))
}

func (a *KBAPI) list(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("list knowledge bases requested")
	// See enactagentapi list: candidates come from the organization, rules
	// decide visibility.
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
	records, err := a.kbs.List(req.Request.Context(), organizationID)
	if err != nil {
		logger.Error("failed to list knowledge bases", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list knowledge bases")
		return
	}
	out := make([]knowledgeBaseResponse, 0, len(records))
	for _, record := range records {
		if !effective.Allows(rbac.Permission(rbac.ResourceKB, rbac.ActionView, record.ID)) {
			continue
		}
		out = append(out, toKBResponse(record))
	}
	logger.Info("knowledge bases listed", "candidates", len(records), "visible", len(out),
		"organization_id", organizationID)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listKBResponse{KnowledgeBases: out})
}

// update partially updates a knowledge base's own fields — currently only
// the friendly name. Documents are deliberately out of scope here.
func (a *KBAPI) update(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id)
	logger.Info("update knowledge base requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionEdit, id); err != nil {
		logger.Warn("update knowledge base denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	record, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up knowledge base for update", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to update knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for update")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base loaded", "name", record.Name)

	var body updateKBRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid update body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("update request decoded", "name_provided", body.Name != nil)

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeError(req, resp, http.StatusBadRequest, "name must not be empty")
			return
		}
		record.Name = name
	}
	record.UpdatedAt = time.Now().UTC()
	if err := a.kbs.Create(req.Request.Context(), record); err != nil {
		logger.Error("failed to update knowledge base", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to update knowledge base")
		return
	}
	logger.Info("knowledge base updated", "name", record.Name)
	requesthelper.WriteJSON(req, resp, http.StatusOK, toKBResponse(record))
}

func (a *KBAPI) get(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id)
	logger.Info("get knowledge base requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionView, id); err != nil {
		logger.Warn("get knowledge base denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}
	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	record, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to get knowledge base", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to get knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base loaded")
	metas, err := a.documents.ListMetaByKB(req.Request.Context(), record.UserID, record.ID)
	if err != nil {
		logger.Error("failed to list knowledge base documents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list knowledge base documents")
		return
	}
	docs := make([]kbDocumentResponse, 0, len(metas))
	for _, m := range metas {
		docs = append(docs, kbDocumentResponse{DocumentID: m.DocumentID, Filename: m.Filename, UploadedAt: m.UploadedAt})
	}
	logger.Info("knowledge base fetched", "documents", len(docs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, kbDetailResponse{
		knowledgeBaseResponse: toKBResponse(record),
		Documents:             docs,
	})
}

// listDocuments returns every document of the KB including its extracted
// text. The caller (in practice the inference service, acting as the agent's
// owner) gets everything needed to place the KB's content in a model prompt.
func (a *KBAPI) listDocuments(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id)
	logger.Info("list knowledge base documents requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionView, id); err != nil {
		logger.Warn("list documents denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	record, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up knowledge base for document listing", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for document listing")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base loaded")

	docs, err := a.documents.ListByKBs(req.Request.Context(), userID, []string{record.ID})
	if err != nil {
		logger.Error("failed to list knowledge base documents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list knowledge base documents")
		return
	}
	out := make([]kbDocumentContentResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, kbDocumentContentResponse{
			DocumentID: d.DocumentID,
			Filename:   d.Filename,
			Text:       d.Text,
			UploadedAt: d.UploadedAt,
		})
	}
	totalChars := 0
	for _, d := range out {
		totalChars += len(d.Text)
	}
	logger.Info("knowledge base documents fetched", "documents", len(out), "total_chars", totalChars)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listKBDocumentsResponse{KBID: record.ID, Documents: out})
}

func (a *KBAPI) delete(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id)
	logger.Info("delete knowledge base requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionDelete, id); err != nil {
		logger.Warn("delete knowledge base denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}
	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	_, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up knowledge base for deletion", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for deletion")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	if err := a.kbs.Delete(req.Request.Context(), id); err != nil {
		logger.Error("failed to delete knowledge base", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete knowledge base")
		return
	}
	logger.Info("knowledge base record deleted")
	// Cascade: remove every document stored under the deleted KB.
	if err := a.documents.DeleteByKB(req.Request.Context(), id); err != nil {
		logger.Error("failed to delete knowledge base documents", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete knowledge base documents")
		return
	}
	logger.Info("knowledge base deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *KBAPI) uploadDocument(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id)
	logger.Info("document upload requested")
	// A document belongs to its knowledge base, so adding one is editing it.
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionEdit, id); err != nil {
		logger.Warn("document upload denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	record, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up knowledge base for upload", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for upload")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base loaded")

	// All files are read and validated up front, so a bad file rejects the
	// whole request before anything is enqueued.
	files, status, msg := requesthelper.ReadUploadedFiles(req, resp, fileFormField, maxUploadBytes)
	if status != 0 {
		logger.Warn("invalid document upload", "err", msg)
		writeError(req, resp, status, msg)
		return
	}
	logger.Info("upload files read", "files", len(files))

	_ = userID // kb.UserID is authoritative for ownership; impersonation header is informational here.
	queued := make([]queuedDocument, 0, len(files))
	for _, f := range files {
		docID := uuid.NewString()
		message := queue.DocumentMessage{
			Type:       queue.DocumentTypeKBContext,
			UserID:     record.UserID,
			KBID:       record.ID,
			DocumentID: docID,
			Filename:   f.Filename,
			// Content carries the raw document bytes base64-encoded; the indexer
			// decodes them and hands the bytes to Tika for text extraction.
			Content: base64.StdEncoding.EncodeToString(f.Content),
		}
		if err := a.producer.Publish(req.Request.Context(), message); err != nil {
			logger.Error("failed to enqueue document for indexing",
				"document_id", docID, "file_name", f.Filename, "queued", len(queued), "total", len(files), "err", err)
			writeError(req, resp, http.StatusBadGateway,
				fmt.Sprintf("failed to enqueue %q for indexing (%d of %d files were queued)", f.Filename, len(queued), len(files)))
			return
		}
		logger.Info("document queued for indexing",
			"document_id", docID, "file_name", f.Filename, "size_bytes", len(f.Content))
		queued = append(queued, queuedDocument{DocumentID: docID, Filename: f.Filename, Status: "queued"})
	}
	logger.Info("document upload accepted", "documents", len(queued))
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, uploadDocumentResponse{
		KBID:      record.ID,
		Documents: queued,
	})
}

// deleteDocument queues the asynchronous removal of one context document;
// the indexer performs the actual delete, scoped to this knowledge base.
func (a *KBAPI) deleteDocument(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	docID := req.PathParameter("docId")
	logger := requesthelper.Logger(req, a.logger).WithFields("kb_id", id, "document_id", docID)
	logger.Info("document deletion requested")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceKB, rbac.ActionEdit, id); err != nil {
		logger.Warn("document deletion denied", "err", err)
		rbac.WriteDenied(req, resp, err, "knowledge base not found")
		return
	}

	organizationID, ok := a.organization(req, resp, "knowledge base not found")
	if !ok {
		return
	}
	record, found, err := a.kbs.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to look up knowledge base for document deletion", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to look up knowledge base")
		return
	}
	if !found {
		logger.Warn("knowledge base not found for document deletion")
		writeError(req, resp, http.StatusNotFound, "knowledge base not found")
		return
	}
	logger.Info("knowledge base loaded")

	message := queue.DocumentMessage{
		Type:       queue.DocumentTypeKBContextDelete,
		UserID:     record.UserID,
		KBID:       record.ID,
		DocumentID: docID,
	}
	if err := a.producer.Publish(req.Request.Context(), message); err != nil {
		logger.Error("failed to enqueue document deletion", "err", err)
		writeError(req, resp, http.StatusBadGateway, "failed to enqueue document deletion")
		return
	}
	logger.Info("document deletion queued")
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, deleteDocumentResponse{
		KBID:       record.ID,
		DocumentID: docID,
		Status:     "queued",
	})
}

func toKBResponse(record kb.KnowledgeBase) knowledgeBaseResponse {
	return knowledgeBaseResponse{
		ID:        record.ID,
		UserID:    record.UserID,
		Name:      record.Name,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
}

func writeError(req *restful.Request, resp *restful.Response, status int, msg string) {
	requesthelper.WriteError(req, resp, status, msg)
}
