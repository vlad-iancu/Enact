// Package kb holds the knowledge-base domain's repositories: knowledge-base
// metadata records and the extracted documents stored under them. It is a
// standalone package (rather than living inside a single service) because
// several services share the domain: the KB API owns KB CRUD and cascades
// deletes to documents, the document indexer stores extracted documents, the
// agent API validates KB references, and the inference service loads a KB's
// documents into the model context.
package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Config holds the OpenSearch index names for the knowledge-base domain.
type Config struct {
	KnowledgeBasesIndex string `env:"OPENSEARCH_INDEX_KNOWLEDGE_BASES, default=enact-knowledge-bases"`
	DocumentsIndex      string `env:"OPENSEARCH_INDEX_KB_DOCUMENTS, default=enact-kb-documents"`
}

// KnowledgeBase is the metadata record for a knowledge base. The id is the
// identifier used across APIs; Name is the user-facing friendly name, given
// at creation and updatable.
type KnowledgeBase struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Repository persists knowledge-base metadata records in OpenSearch.
type Repository struct {
	os    *opensearch.Client
	index string
}

// NewRepository returns a knowledge-base Repository using the index in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.KnowledgeBasesIndex}
}

// EnsureIndex verifies the knowledge-base index exists. The index and its
// mapping are owned by the composable index template in mappings/ and created
// by `make infrastructure-up`; this fails fast when it is missing.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("kb: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Create persists a knowledge base record.
func (r *Repository) Create(ctx context.Context, kb KnowledgeBase) error {
	body, err := json.Marshal(kb)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, kb.ID, body)
}

// Get fetches a knowledge base by id. The boolean reports existence.
func (r *Repository) Get(ctx context.Context, id string) (KnowledgeBase, bool, error) {
	var kb KnowledgeBase
	found, err := r.os.GetSource(ctx, r.index, id, &kb)
	return kb, found, err
}

// List returns all knowledge bases owned by userID.
func (r *Repository) List(ctx context.Context, userID string) ([]KnowledgeBase, error) {
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"user_id": userID}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("kb: marshal list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]KnowledgeBase, 0, len(hits))
	for _, h := range hits {
		var kb KnowledgeBase
		if err := json.Unmarshal(h.Source, &kb); err != nil {
			return nil, err
		}
		out = append(out, kb)
	}
	return out, nil
}

// Delete removes a knowledge base record. It does NOT touch the chunks
// indexed under the KB; callers compose this with ChunkRepository.DeleteByKB
// for the cascade.
func (r *Repository) Delete(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.index, id)
}
