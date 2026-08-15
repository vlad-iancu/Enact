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
	UserID     string `json:"user_id"`
	AgentID    string `json:"agent_id"`
	DocumentID string `json:"document_id"`
	ChunkIndex int    `json:"chunk_index"`
	// Filename of the uploaded document, denormalized onto every chunk so
	// document listings can show it. Chunks indexed before this field
	// existed have it empty.
	Filename  string    `json:"filename,omitempty"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding"`
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

// RAGDocument summarizes one uploaded document of an agent's RAG
// collection: identity, display name, and how many chunks it produced.
type RAGDocument struct {
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Chunks     int    `json:"chunks"`
}

// ListDocuments returns the distinct documents of an agent's RAG collection
// via a terms aggregation on document_id (capped at 1000 distinct documents,
// the platform's usual listing ceiling). Filename comes from a one-hit
// top_hits sample per bucket; chunks indexed before filenames existed yield
// an empty one.
func (r *RAGRepository) ListDocuments(ctx context.Context, userID, agentID string) ([]RAGDocument, error) {
	body, err := json.Marshal(map[string]any{
		"size": 0,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"user_id": userID}},
					map[string]any{"term": map[string]any{"agent_id": agentID}},
				},
			},
		},
		"aggs": map[string]any{
			"documents": map[string]any{
				"terms": map[string]any{"field": "document_id", "size": 1000},
				"aggs": map[string]any{
					"sample": map[string]any{
						"top_hits": map[string]any{"size": 1, "_source": []string{"filename"}},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agents: marshal list rag documents query: %w", err)
	}
	res, err := r.os.SearchWithAggregations(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	if res.Aggregations == nil {
		return []RAGDocument{}, nil
	}
	var aggs struct {
		Documents struct {
			Buckets []struct {
				Key      string `json:"key"`
				DocCount int    `json:"doc_count"`
				Sample   struct {
					Hits struct {
						Hits []struct {
							Source struct {
								Filename string `json:"filename"`
							} `json:"_source"`
						} `json:"hits"`
					} `json:"hits"`
				} `json:"sample"`
			} `json:"buckets"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(res.Aggregations, &aggs); err != nil {
		return nil, fmt.Errorf("agents: decode rag document aggregation: %w", err)
	}
	out := make([]RAGDocument, 0, len(aggs.Documents.Buckets))
	for _, b := range aggs.Documents.Buckets {
		doc := RAGDocument{DocumentID: b.Key, Chunks: b.DocCount}
		if hits := b.Sample.Hits.Hits; len(hits) > 0 {
			doc.Filename = hits[0].Source.Filename
		}
		out = append(out, doc)
	}
	return out, nil
}

// DeleteByDocument removes one document's chunks, scoped to its agent: the
// agent_id filter makes deleting another agent's document by guessed id
// structurally impossible. Idempotent — zero matches is success.
func (r *RAGRepository) DeleteByDocument(ctx context.Context, agentID, documentID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"agent_id": agentID}},
					map[string]any{"term": map[string]any{"document_id": documentID}},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("agents: marshal delete rag document query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
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
