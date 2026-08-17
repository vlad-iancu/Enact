package enactmain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/conversations"
	"enact/internal/identity"
	"enact/internal/inference"
	"enact/internal/requesthelper"
)

// titleMaxChars caps the conversation title derived from the first message.
const titleMaxChars = 60

type createConversationResponse struct {
	ID string `json:"id"`
}

type listConversationsResponse struct {
	Conversations []conversations.Summary `json:"conversations"`
}

// updateConversationRequest is the partial-update body for a conversation's
// own fields — currently only the title. Messages are appended via their own
// endpoint, never edited here.
type updateConversationRequest struct {
	Title *string `json:"title"`
}

// addMessageRequest posts one user message. Exactly one of AgentID or Model
// selects what answers it — chosen per message, because conversations are
// deliberately not bound to either.
type addMessageRequest struct {
	Content       string `json:"content"`
	AgentID       string `json:"agent_id,omitempty"`
	Model         string `json:"model,omitempty"`
	RetrievalTopK *int   `json:"retrieval_top_k,omitempty"`
	// ContextFiles are forwarded to the inference service, which passes
	// them to the model natively (Bedrock DocumentBlocks) for THIS turn.
	// Only their filenames are persisted on the message.
	ContextFiles []inference.ContextFile `json:"context_files,omitempty"`
}

// maxMessageContextFiles mirrors the inference service's cap so the obvious
// violation fails as a clean 400 before the SSE stream starts.
const maxMessageContextFiles = 5

// conversationsWebService returns the session-guarded conversation routes.
func (a *MainAPI) conversationsWebService() *restful.WebService {
	ws := new(restful.WebService)
	// Adding a message streams SSE, so the group advertises both media
	// types; without it a client sending Accept: text/event-stream is
	// refused with 406.
	ws.Path("/conversations").Produces(restful.MIME_JSON, "text/event-stream")
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.GET("").
		To(a.listConversations).
		Doc("List the logged-in user's conversations, most recently updated first (messages excluded)").
		Returns(http.StatusOK, "OK", listConversationsResponse{}))

	ws.Route(ws.POST("").
		To(a.createConversation).
		Doc("Create an empty conversation").
		Returns(http.StatusCreated, "Created", createConversationResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateConversation).
		Consumes(restful.MIME_JSON).
		Reads(updateConversationRequest{}).
		Param(ws.PathParameter("id", "conversation id")).
		Doc("Partially update a conversation's own fields (currently: title)").
		Returns(http.StatusOK, "OK", conversations.Summary{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteConversation).
		Param(ws.PathParameter("id", "conversation id")).
		Doc("Delete a conversation and its messages, including the tool calls recorded in them. Permanent").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.getConversation).
		Param(ws.PathParameter("id", "conversation id")).
		Doc("Get a conversation with its full message history").
		Returns(http.StatusOK, "OK", conversations.Conversation{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/messages").
		To(a.addMessage).
		Consumes(restful.MIME_JSON).
		Reads(addMessageRequest{}).
		Param(ws.PathParameter("id", "conversation id")).
		Doc("Add a user message; the assistant's reply streams back as SSE and the conversation is saved when the stream completes").
		Returns(http.StatusNotFound, "Not found", errorResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	return ws
}

// requireSession rejects unauthenticated requests; the session is attached
// to the request context's identity so downstream service calls (inference)
// carry the logged-in user, scoping agents and KBs to their owner.
func (a *MainAPI) requireSession(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
	sess, ok := a.session(req)
	if !ok {
		requesthelper.WriteError(req, resp, http.StatusUnauthorized, "not logged in")
		return
	}
	ctx := identity.WithUserID(req.Request.Context(), sess.UserID)
	req.SetAttribute("session", sess)
	req.Request = req.Request.WithContext(ctx)
	chain.ProcessFilter(req, resp)
}

// sessionAttr returns the session stored by requireSession.
func sessionAttr(req *restful.Request) Session {
	sess, _ := req.Attribute("session").(Session)
	return sess
}

func (a *MainAPI) listConversations(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list conversations requested")
	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	list, err := a.conversations.ListByUser(req.Request.Context(), organizationID, sess.UserID)
	if err != nil {
		logger.Error("failed to list conversations", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	logger.Info("conversations listed", "count", len(list))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listConversationsResponse{Conversations: list})
}

// conversationOrganization resolves the caller's organization for a scoped
// lookup, writing the refusal itself when they have none. Conversations are
// private to a person, but the organization is the outer boundary — see
// conversations.Repository.Get.
func (a *MainAPI) conversationOrganization(req *restful.Request, resp *restful.Response) (string, bool) {
	effective, err := a.rbac.Effective(req.Request.Context(), sessionAttr(req).UserID)
	if err != nil {
		relayRBACErr(req, resp, err, "resolve your organization")
		return "", false
	}
	if effective.OrganizationID == "" {
		requesthelper.WriteError(req, resp, http.StatusForbidden,
			"you do not belong to an organization yet; request one and ask an administrator to approve it")
		return "", false
	}
	return effective.OrganizationID, true
}

func (a *MainAPI) createConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create conversation requested")
	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	now := time.Now().UTC()
	conv := conversations.Conversation{
		ID:             uuid.NewString(),
		UserID:         sess.UserID,
		OrganizationID: organizationID,
		Title:          "New conversation",
		Messages:       []conversations.Message{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.conversations.Save(req.Request.Context(), conv); err != nil {
		logger.Error("failed to create conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create conversation")
		return
	}
	logger.Info("conversation created", "conversation_id", conv.ID)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, createConversationResponse{ID: conv.ID})
}

func (a *MainAPI) getConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "conversation_id", id)
	logger.Info("get conversation requested")
	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	conv, found, err := a.conversations.Get(req.Request.Context(), organizationID, sess.UserID, id)
	if err != nil {
		logger.Error("failed to get conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to get conversation")
		return
	}
	if !found {
		logger.Warn("conversation not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "conversation not found")
		return
	}
	logger.Info("conversation fetched", "messages", conv.MessageCount)
	requesthelper.WriteJSON(req, resp, http.StatusOK, conv)
}

// updateConversation partially updates a conversation's own fields —
// currently only the title. A custom title also stops the automatic
// first-message title derivation from ever overwriting it.
// deleteConversation removes one of the caller's own conversations.
//
// A hard delete, not a flag: the record holds the user's messages and, since
// ADR-0018, the verbatim results of every tool call made on their behalf.
// "Deleted" has to mean gone for that to be an honest answer.
func (a *MainAPI) deleteConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "conversation_id", id)
	logger.Info("delete conversation requested")

	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	deleted, err := a.conversations.Delete(req.Request.Context(), organizationID, sess.UserID, id)
	if err != nil {
		logger.Error("failed to delete conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete conversation")
		return
	}
	if !deleted {
		// Somebody else's conversation, another organization's, or none at
		// all — all indistinguishable on purpose.
		logger.Warn("conversation not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "conversation not found")
		return
	}
	logger.Info("conversation deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) updateConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "conversation_id", id)
	logger.Info("update conversation requested")

	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	conv, found, err := a.conversations.Get(req.Request.Context(), organizationID, sess.UserID, id)
	if err != nil {
		logger.Error("failed to load conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load conversation")
		return
	}
	if !found {
		logger.Warn("conversation not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "conversation not found")
		return
	}
	logger.Info("conversation loaded", "title", conv.Title)

	var body updateConversationRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid update body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("update request decoded", "title_provided", body.Title != nil)

	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			requesthelper.WriteError(req, resp, http.StatusBadRequest, "title must not be empty")
			return
		}
		conv.Title = title
	}
	conv.UpdatedAt = time.Now().UTC()
	if err := a.conversations.Save(req.Request.Context(), conv); err != nil {
		logger.Error("failed to save conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to update conversation")
		return
	}
	logger.Info("conversation updated", "title", conv.Title)
	requesthelper.WriteJSON(req, resp, http.StatusOK, conversations.Summary{
		ID:           conv.ID,
		Title:        conv.Title,
		MessageCount: len(conv.Messages),
		CreatedAt:    conv.CreatedAt,
		UpdatedAt:    conv.UpdatedAt,
	})
}

// addMessage appends the user's message, streams the assistant's reply back
// as SSE (relayed from the inference service), and saves the conversation
// once the stream completes. The assistant text accumulated so far is saved
// even when the upstream stream fails part-way, so nothing typed or
// generated is silently lost.
func (a *MainAPI) addMessage(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "conversation_id", id)
	logger.Info("add message requested")

	var body addMessageRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid message body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	attachmentNames := make([]string, 0, len(body.ContextFiles))
	for _, f := range body.ContextFiles {
		attachmentNames = append(attachmentNames, f.Filename)
	}
	logger.Info("message decoded", "content_chars", len(body.Content), "agent_id", body.AgentID, "model", body.Model, "retrieval_top_k", body.RetrievalTopK, "attachments", attachmentNames)

	if strings.TrimSpace(body.Content) == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "content must not be empty")
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

	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	conv, found, err := a.conversations.Get(req.Request.Context(), organizationID, sess.UserID, id)
	if err != nil {
		logger.Error("failed to load conversation", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load conversation")
		return
	}
	if !found {
		logger.Warn("conversation not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "conversation not found")
		return
	}
	logger.Info("conversation loaded", "messages", conv.MessageCount)

	now := time.Now().UTC()
	conv.Messages = append(conv.Messages, conversations.Message{
		Role:        "user",
		Content:     body.Content,
		AgentID:     body.AgentID,
		Model:       body.Model,
		Attachments: attachmentNames,
		CreatedAt:   now,
	})
	if conv.Title == "" || conv.Title == "New conversation" {
		conv.Title = deriveTitle(body.Content)
	}

	// Full history goes to the model so the chat has memory across turns.
	infReq := inference.Request{
		AgentID:       body.AgentID,
		Model:         body.Model,
		RetrievalTopK: body.RetrievalTopK,
		// Files attach to the last user message downstream — the one just
		// appended. Only this turn carries them; history replays never do.
		ContextFiles: body.ContextFiles,
		Messages:     make([]inference.Message, 0, len(conv.Messages)),
	}
	for _, m := range conv.Messages {
		// Tool calls travel with the message that made them, so the model
		// sees what it already did — and does not redo it.
		calls := make([]inference.MessageToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			calls = append(calls, inference.MessageToolCall{
				ServerID:          c.ServerID,
				Tool:              c.Tool,
				ToolUseID:         c.ToolUseID,
				Arguments:         c.Arguments,
				Content:           c.Content,
				StructuredContent: c.StructuredContent,
				IsError:           c.IsError,
				Turn:              c.Turn,
				Text:              c.Text,
			})
		}
		infReq.Messages = append(infReq.Messages, inference.Message{Role: m.Role, Content: m.Content, ToolCalls: calls})
	}

	// The meta event carries the conversation id so a client that just
	// created the conversation has it without a second request.
	out, streamErr := a.relayInferenceStream(req, resp, logger, infReq, map[string]any{"conversation_id": conv.ID})
	if streamErr != nil {
		logger.Warn("stream ended with error; persisting partial result", "assistant_chars", len(out.Assistant))
	}
	logger.Info("stream relayed", "assistant_chars", len(out.Assistant), "tool_calls", len(out.ToolCalls))

	// Persist: the user message always; the assistant message when any text
	// arrived. Saving must survive the browser hanging up mid-stream, so it
	// does not use the (possibly cancelled) request context.
	// Tool calls alone are worth an assistant message: a turn that ran tools
	// and then lost the connection still happened, and the record of what it
	// did should survive.
	if len(out.Assistant) > 0 || len(out.ToolCalls) > 0 {
		conv.Messages = append(conv.Messages, conversations.Message{
			Role:      "assistant",
			Content:   out.Assistant,
			AgentID:   body.AgentID,
			Model:     body.Model,
			ToolCalls: out.ToolCalls,
			CreatedAt: time.Now().UTC(),
		})
	}
	conv.UpdatedAt = time.Now().UTC()
	saveCtx, cancel := saveContext()
	defer cancel()
	if err := a.conversations.Save(saveCtx, conv); err != nil {
		logger.Error("failed to save conversation", "err", err, "messages", len(conv.Messages))
		return
	}
	logger.Info("conversation saved", "messages", len(conv.Messages))
}

// saveContext returns a short standalone context for persisting a finished
// conversation: the request context may already be cancelled (browser gone),
// and the accumulated messages must be saved regardless.
func saveContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// deriveTitle makes a listing title from the first message.
func deriveTitle(content string) string {
	title := strings.Join(strings.Fields(content), " ")
	if len(title) > titleMaxChars {
		title = title[:titleMaxChars] + "…"
	}
	return title
}
