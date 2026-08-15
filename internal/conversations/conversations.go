// Package conversations holds the chat-conversation domain: per-user
// threads of messages between the user and the chatbot, persisted in
// OpenSearch. A conversation is deliberately NOT bound to an agent or a
// model — the user may switch either per message, so each assistant message
// records what produced it.
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Config holds the OpenSearch index name for the conversation domain.
type Config struct {
	Index string `env:"OPENSEARCH_INDEX_CONVERSATIONS, default=enact-conversations"`
}

// Message is one turn in a conversation. Role is "user" or "assistant";
// AgentID/Model record what an assistant message was produced by (and, on a
// user message, what it was addressed to).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	AgentID string `json:"agent_id,omitempty"`
	Model   string `json:"model,omitempty"`
	// Attachments are the filenames of context files sent with this message.
	// Only the names are persisted: the files are read by the model on the
	// turn they were attached to; later turns do not re-send them.
	Attachments []string `json:"attachments,omitempty"`
	// ToolCalls are the MCP tools this assistant message used, in the order
	// they ran, so reopening a conversation shows what the agent actually
	// did rather than only what it concluded.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// ToolCall is one MCP tool invocation recorded on an assistant message.
//
// Arguments are the MODEL's arguments, exactly as the stream reported them —
// before the platform injects any credential — so a stored conversation can
// never carry a user's token. Everything else is kept as the tool returned
// it, because the record has to be able to reproduce the exchange rather
// than merely describe it.
type ToolCall struct {
	ServerID  string          `json:"server_id"`
	Tool      string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Content and StructuredContent are what the tool returned. A server may
	// send prose, a structured object, or both; both are kept, because both
	// are what the model was given.
	Content           string          `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	// Turn is the tool-loop round this call belongs to, counting from 1, and
	// Text is what the assistant said in that round before calling. Together
	// they make the record a transcript rather than a summary: a second
	// round's call was made KNOWING the first round's results, and replaying
	// them as one round would misrepresent that.
	Turn int    `json:"turn,omitempty"`
	Text string `json:"text,omitempty"`
}

// Conversation is one user's message thread. MessageCount is denormalized so
// listings (which exclude the messages themselves) can still show it.
type Conversation struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	Title        string    `json:"title"`
	MessageCount int       `json:"message_count"`
	Messages     []Message `json:"messages"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Summary is the listing shape: everything but the messages.
type Summary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Repository persists conversations in OpenSearch.
type Repository struct {
	os    *opensearch.Client
	index string
}

// NewRepository returns a conversation Repository using the index in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.Index}
}

// EnsureIndex verifies the conversations index exists. The index and its
// mapping are owned by the composable index template in mappings/ and
// created by `make infrastructure-up`; this fails fast when it is missing.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("conversations: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Get fetches a conversation by id, scoped to its owner: a conversation
// belonging to another user is reported as not found, so existence is not
// leaked across users. Lookup is realtime (GET by id).
func (r *Repository) Get(ctx context.Context, userID, id string) (Conversation, bool, error) {
	var c Conversation
	found, err := r.os.GetSource(ctx, r.index, id, &c)
	if err != nil || !found {
		return Conversation{}, false, err
	}
	if c.UserID != userID {
		return Conversation{}, false, nil
	}
	return c, true, nil
}

// ListByUser returns the user's conversation summaries, most recently
// updated first. The (potentially large) message bodies are excluded at the
// OpenSearch level.
func (r *Repository) ListByUser(ctx context.Context, userID string) ([]Summary, error) {
	body, err := json.Marshal(map[string]any{
		"size":    1000,
		"_source": map[string]any{"excludes": []string{"messages"}},
		"query":   map[string]any{"term": map[string]any{"user_id": userID}},
		"sort":    []any{map[string]any{"updated_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("conversations: marshal list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(hits))
	for _, h := range hits {
		var s Summary
		if err := json.Unmarshal(h.Source, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Save persists a conversation, overwriting any previous version and
// keeping the denormalized message count in step.
func (r *Repository) Save(ctx context.Context, c Conversation) error {
	c.MessageCount = len(c.Messages)
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, c.ID, body)
}
