package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"enact/internal/opensearch"
)

// RAGChunk is a single indexed slice of a document uploaded to an agent's
// RAG configuration, including its embedding.
type RAGChunk struct {
	UserID     string    `json:"user_id"`
	AgentID    string    `json:"agent_id"`
	DocumentID string    `json:"document_id"`
	ChunkIndex int       `json:"chunk_index"`
	Text       string    `json:"text"`
	Embedding  []float32 `json:"embedding"`
}

// RetrievedChunk pairs a RAG chunk with its relevance score.
type RetrievedChunk struct {
	RAGChunk
	Score float64
}

// RAGRepository persists and retrieves the chunks of an agent's RAG
// configuration. Every agent has exactly one RAG collection, scoped by its
// agent id; documents are uploaded to it separately from knowledge bases.
type RAGRepository struct {
	os    *opensearch.Client
	index string
}

// NewRAGRepository returns a RAGRepository using the index in cfg.
func NewRAGRepository(os *opensearch.Client, cfg Config) *RAGRepository {
	return &RAGRepository{os: os, index: cfg.RAGChunksIndex}
}

// EnsureIndex verifies the RAG chunk index exists. The index and its mapping
// are owned by the composable index template in mappings/ and created by
// `make infrastructure-up`; this fails fast when it is missing so a service
// does not silently run against a misconfigured (e.g. non-k-NN) index.
func (r *RAGRepository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("agents: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Index indexes a single RAG chunk and its embedding. The document id is
// composed from the document id and chunk index so re-indexing the same
// document overwrites rather than duplicates.
func (r *RAGRepository) Index(ctx context.Context, c RAGChunk) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%d", c.DocumentID, c.ChunkIndex)
	return r.os.IndexDoc(ctx, r.index, id, body)
}

// Search runs a k-NN search over the agent's RAG collection, returning the k
// most similar chunks.
func (r *RAGRepository) Search(ctx context.Context, userID, agentID string, vector []float32, k int) ([]RetrievedChunk, error) {
	if agentID == "" || k <= 0 {
		return nil, nil
	}
	filter := []any{
		map[string]any{"term": map[string]any{"user_id": userID}},
		map[string]any{"term": map[string]any{"agent_id": agentID}},
	}
	body, err := json.Marshal(map[string]any{
		"size": k,
		"query": map[string]any{
			"knn": map[string]any{
				"embedding": map[string]any{
					"vector": vector,
					"k":      k,
					"filter": map[string]any{"bool": map[string]any{"filter": filter}},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agents: marshal rag search query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]RetrievedChunk, 0, len(hits))
	for _, h := range hits {
		var c RAGChunk
		if err := json.Unmarshal(h.Source, &c); err != nil {
			return nil, err
		}
		out = append(out, RetrievedChunk{RAGChunk: c, Score: h.Score})
	}
	return out, nil
}

// HasChunks reports whether the agent's RAG collection contains any chunks.
// It is a cheap existence probe (no scoring, no source fetch) used to skip
// query embedding for agents without RAG documents.
func (r *RAGRepository) HasChunks(ctx context.Context, userID, agentID string) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"size":    1,
		"_source": false,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"user_id": userID}},
					map[string]any{"term": map[string]any{"agent_id": agentID}},
				},
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("agents: marshal rag exists query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return false, err
	}
	return len(hits) > 0, nil
}

// DeleteByAgent removes every chunk in the given agent's RAG collection.
func (r *RAGRepository) DeleteByAgent(ctx context.Context, agentID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"agent_id": agentID}},
	})
	if err != nil {
		return fmt.Errorf("agents: marshal rag delete query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}
