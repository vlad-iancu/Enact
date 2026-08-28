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

// Config holds the OpenSearch index name for the agent domain. Retrieval
// chunks are the knowledge-base domain's (see kb.Config.ChunksIndex).
type Config struct {
	Index string `env:"OPENSEARCH_INDEX_AGENTS, default=enact-agents"`
}

// Agent is a configured assistant: a user-facing friendly name, a model, a
// system prompt, and the knowledge bases it retrieves from.
type Agent struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// OrganizationID is the organization this agent belongs to. Stored rather
	// than inferred from the owner: every read compares it, and an owner
	// bypasses permission checks, so this is the only thing keeping one
	// organization out of another's data.
	OrganizationID string `json:"organization_id"`

	Name             string   `json:"name"`
	Model            string   `json:"model"`
	SystemPrompt     string   `json:"system_prompt"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	// Tools names the MCP servers (by registry id) whose tools this agent
	// may call during inference.
	Tools []string `json:"tools"`
	// RAGKnowledgeBaseID is the ONE retrieval knowledge base this agent draws
	// passages from, or empty for none.
	//
	// One rather than many, deliberately. Retrieval ranks passages by distance
	// within a single collection; searching several and merging their scores
	// compares numbers that were never comparable, and the usual result is one
	// corpus quietly crowding out another. A single collection keeps the
	// ranking meaningful — combine sources by putting them in one knowledge
	// base, where they are embedded the same way.
	//
	// Distinct from KnowledgeBaseIDs above, which are CONTEXT knowledge bases:
	// those are loaded whole, and there is no ranking to confuse.
	RAGKnowledgeBaseID string `json:"rag_knowledge_base_id,omitempty"`
	// OutputSchema constrains the assistant's own reply to JSON matching this
	// JSON Schema, via Bedrock's structured output. Absent — the default —
	// means ordinary prose.
	//
	// Stored as raw JSON and passed to Bedrock verbatim: it is the caller's
	// schema, and reserializing it through a Go type would silently drop the
	// keywords we do not model. The agent API checks it is a JSON object and
	// bounds its size; what the keywords mean is Bedrock's business.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
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

// Get fetches one agent by id, scoped to an organization.
//
// An agent belonging to a different organization is reported as ABSENT
// rather than refused: callers already render that as 404, and "not yours"
// must be indistinguishable from "does not exist".
//
// The organization is a parameter rather than something the caller checks
// afterwards because an organization owner passes every permission check by
// construction — so if this returned the record and left the comparison to
// the handler, one forgotten comparison would expose another organization's
// data. Here it cannot be forgotten.
func (r *Repository) Get(ctx context.Context, organizationID, id string) (Agent, bool, error) {
	var a Agent
	found, err := r.os.GetSource(ctx, r.index, id, &a)
	if err != nil || !found {
		return Agent{}, found, err
	}
	if organizationID == "" || a.OrganizationID != organizationID {
		return Agent{}, false, nil
	}
	return a, true, nil
}

// List returns the agents owned by any of userIDs — the caller's whole
// organization, since a resource's organization is its owner's. The caller
// then drops what their rules do not cover; ownership alone is no longer the
// filter. An empty list matches nothing, which is the correct answer for a
// user who belongs to no organization.
func (r *Repository) List(ctx context.Context, organizationID string) ([]Agent, error) {
	if organizationID == "" {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
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
