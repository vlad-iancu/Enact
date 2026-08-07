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
}

// conversationsWebService returns the session-guarded conversation routes.
func (a *MainAPI) conversationsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/conversations").Produces(restful.MIME_JSON)
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
	list, err := a.conversations.ListByUser(req.Request.Context(), sess.UserID)
	if err != nil {
		logger.Error("failed to list conversations", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	logger.Info("conversations listed", "count", len(list))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listConversationsResponse{Conversations: list})
}

func (a *MainAPI) createConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create conversation requested")
	now := time.Now().UTC()
	conv := conversations.Conversation{
		ID:        uuid.NewString(),
		UserID:    sess.UserID,
		Title:     "New conversation",
		Messages:  []conversations.Message{},
		CreatedAt: now,
		UpdatedAt: now,
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
	conv, found, err := a.conversations.Get(req.Request.Context(), sess.UserID, id)
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
func (a *MainAPI) updateConversation(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "conversation_id", id)
	logger.Info("update conversation requested")

	conv, found, err := a.conversations.Get(req.Request.Context(), sess.UserID, id)
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
	logger.Info("message decoded", "content_chars", len(body.Content), "agent_id", body.AgentID, "model", body.Model, "retrieval_top_k", body.RetrievalTopK)

	if strings.TrimSpace(body.Content) == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "content must not be empty")
		return
	}
	if (body.AgentID == "") == (body.Model == "") {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "exactly one of agent_id or model must be set")
		return
	}

	conv, found, err := a.conversations.Get(req.Request.Context(), sess.UserID, id)
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
		Role:      "user",
		Content:   body.Content,
		AgentID:   body.AgentID,
		Model:     body.Model,
		CreatedAt: now,
	})
	if conv.Title == "" || conv.Title == "New conversation" {
		conv.Title = deriveTitle(body.Content)
	}

	// Full history goes to the model so the chat has memory across turns.
	infReq := inference.Request{
		AgentID:       body.AgentID,
		Model:         body.Model,
		RetrievalTopK: body.RetrievalTopK,
		Messages:      make([]inference.Message, 0, len(conv.Messages)),
	}
	for _, m := range conv.Messages {
		infReq.Messages = append(infReq.Messages, inference.Message{Role: m.Role, Content: m.Content})
	}

	flusher, ok := resp.ResponseWriter.(http.Flusher)
	if !ok {
		logger.Error("streaming not supported by underlying writer")
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "streaming not supported")
		return
	}
	resp.Header().Set("Content-Type", "text/event-stream")
	resp.Header().Set("Cache-Control", "no-cache")
	resp.Header().Set("X-Accel-Buffering", "no")
	meta := requesthelper.MetaFrom(req.Request.Context())
	resp.Header().Set("X-Trace-Id", meta.TraceID)
	resp.WriteHeader(http.StatusOK)
	// The first SSE event carries the conversation id and trace id, so a
	// client that just created the conversation (or a browser that cannot
	// read headers) has both without a second request.
	if metaBody, err := json.Marshal(map[string]any{"conversation_id": conv.ID, "trace_id": meta.TraceID}); err == nil {
		_, _ = fmt.Fprintf(resp, "event: meta\ndata: %s\n\n", metaBody)
		flusher.Flush()
	}

	// Relay the upstream SSE stream, accumulating the assistant's text.
	var assistant strings.Builder
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
		default:
			// Upstream meta (inference's own trace event) is not relayed:
			// this response has its own meta event of the same trace.
			return nil
		}
	})
	if streamErr != nil {
		logger.Error("inference stream failed", "err", streamErr, "assistant_chars", assistant.Len())
		errBody, _ := json.Marshal(map[string]any{"error": streamErr.Error(), "meta": requesthelper.MetaFrom(req.Request.Context())})
		_, _ = fmt.Fprintf(resp, "event: error\ndata: %s\n\n", errBody)
		flusher.Flush()
	}
	logger.Info("stream relayed", "assistant_chars", assistant.Len())

	// Persist: the user message always; the assistant message when any text
	// arrived. Saving must survive the browser hanging up mid-stream, so it
	// does not use the (possibly cancelled) request context.
	if assistant.Len() > 0 {
		conv.Messages = append(conv.Messages, conversations.Message{
			Role:      "assistant",
			Content:   assistant.String(),
			AgentID:   body.AgentID,
			Model:     body.Model,
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
