// Package agents holds the agent domain's repository: it persists agent
// records in OpenSearch. It is a standalone package (rather than living
// inside a single service) because two services share the domain: the agent
// API owns the CRUD and the inference service resolves agents by id.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Config holds the OpenSearch index names for the agent domain: the agent
// records themselves and the RAG chunk collections uploaded to agents.
type Config struct {
	Index          string `env:"OPENSEARCH_INDEX_AGENTS, default=enact-agents"`
	RAGChunksIndex string `env:"OPENSEARCH_INDEX_AGENT_RAG_CHUNKS, default=enact-agent-rag-chunks"`
}

// Agent is a configured assistant: a model (friendly name), a system prompt,
// and the knowledge bases it retrieves from.
type Agent struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	Model            string    `json:"model"`
	SystemPrompt     string    `json:"system_prompt"`
	KnowledgeBaseIDs []string  `json:"knowledge_base_ids"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Repository persists agent records in OpenSearch.
type Repository struct {
	os    *opensearch.Client
	index string
}

// NewRepository returns a Repository using the index in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.Index}
}

// EnsureIndex verifies the agent index exists. The index and its mapping are
// owned by the composable index template in mappings/ and created by
// `make infrastructure-up`; this fails fast when it is missing.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("agents: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Create persists an agent record.
func (r *Repository) Create(ctx context.Context, a Agent) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, a.ID, body)
}

// Get fetches an agent by id. The boolean reports existence.
func (r *Repository) Get(ctx context.Context, id string) (Agent, bool, error) {
	var a Agent
	found, err := r.os.GetSource(ctx, r.index, id, &a)
	return a, found, err
}

// List returns all agents owned by userID.
func (r *Repository) List(ctx context.Context, userID string) ([]Agent, error) {
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"user_id": userID}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("agents: marshal list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(hits))
	for _, h := range hits {
		var a Agent
		if err := json.Unmarshal(h.Source, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Update replaces an existing agent record (full update).
func (r *Repository) Update(ctx context.Context, a Agent) error {
	return r.Create(ctx, a)
}

// Delete removes an agent record.
func (r *Repository) Delete(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.index, id)
}
