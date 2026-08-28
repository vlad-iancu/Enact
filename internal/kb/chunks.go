package kb

import (
	"context"
	"encoding/json"
	"fmt"

	"enact/internal/opensearch"
)

// Chunk is one indexed slice of a document uploaded to a retrieval knowledge
// base, with its embedding.
//
// Scoped by KB id alone. It used to carry a user id as well, from when a RAG
// collection belonged to one agent and therefore to one person — but a
// knowledge base is shared through RBAC rules, so filtering by the reader's
// own id would hide a colleague's documents in a KB they were given.
// Authorization lives on the knowledge base; this index just holds its bytes.
type Chunk struct {
	KBID       string `json:"kb_id"`
	DocumentID string `json:"document_id"`
	ChunkIndex int    `json:"chunk_index"`
	// Filename of the uploaded document, denormalized onto every chunk so
	// document listings can show it without a second lookup.
	Filename  string    `json:"filename,omitempty"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding"`
}

// RetrievedChunk pairs a chunk with its relevance score.
type RetrievedChunk struct {
	Chunk
	Score float64
}

// ChunkRepository persists and retrieves the chunks of retrieval knowledge
// bases.
type ChunkRepository struct {
	os    *opensearch.Client
	index string
}

func NewChunkRepository(os *opensearch.Client, cfg Config) *ChunkRepository {
	return &ChunkRepository{os: os, index: cfg.ChunksIndex}
}

// EnsureIndex verifies the chunk index exists. The index and its mapping are
// owned by the composable template in mappings/ and created by
// `make infrastructure-up`; this fails fast when it is missing so a service
// does not silently run against a misconfigured (e.g. non-k-NN) index.
func (r *ChunkRepository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("kb: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// ChunkedDocument summarizes one uploaded document: identity, display name,
// and how many chunks it produced.
type ChunkedDocument struct {
	DocumentID string `json:"document_id"`
	Filename   string `json:"filename,omitempty"`
	Chunks     int    `json:"chunks"`
}

// ListDocuments returns the distinct documents of a knowledge base via a terms
// aggregation on document_id (capped at 1000, the platform's usual listing
// ceiling). Filename comes from a one-hit top_hits sample per bucket.
func (r *ChunkRepository) ListDocuments(ctx context.Context, kbID string) ([]ChunkedDocument, error) {
	if kbID == "" {
		return []ChunkedDocument{}, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  0,
		"query": map[string]any{"term": map[string]any{"kb_id": kbID}},
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
		return nil, fmt.Errorf("kb: marshal list chunked documents query: %w", err)
	}
	res, err := r.os.SearchWithAggregations(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	if res.Aggregations == nil {
		return []ChunkedDocument{}, nil
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
		return nil, fmt.Errorf("kb: decode chunked document aggregation: %w", err)
	}
	out := make([]ChunkedDocument, 0, len(aggs.Documents.Buckets))
	for _, b := range aggs.Documents.Buckets {
		doc := ChunkedDocument{DocumentID: b.Key, Chunks: b.DocCount}
		if hits := b.Sample.Hits.Hits; len(hits) > 0 {
			doc.Filename = hits[0].Source.Filename
		}
		out = append(out, doc)
	}
	return out, nil
}

// DeleteByDocument removes one document's chunks, scoped to its knowledge
// base: the kb_id filter makes deleting another KB's document by guessed id
// structurally impossible. Idempotent — zero matches is success.
func (r *ChunkRepository) DeleteByDocument(ctx context.Context, kbID, documentID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"kb_id": kbID}},
					map[string]any{"term": map[string]any{"document_id": documentID}},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("kb: marshal delete chunked document query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}

// Index indexes a single chunk and its embedding. The document id is composed
// from the KB, the document and the chunk index, so re-indexing the same
// document overwrites rather than duplicates — and two knowledge bases holding
// the same document id do not overwrite each other.
func (r *ChunkRepository) Index(ctx context.Context, c Chunk) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%s-%d", c.KBID, c.DocumentID, c.ChunkIndex)
	return r.os.IndexDoc(ctx, r.index, id, body)
}

// Search runs a k-NN search over one knowledge base, returning the k most
// similar chunks.
func (r *ChunkRepository) Search(ctx context.Context, kbID string, vector []float32, k int) ([]RetrievedChunk, error) {
	if kbID == "" || k <= 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size": k,
		"query": map[string]any{
			"knn": map[string]any{
				"embedding": map[string]any{
					"vector": vector,
					"k":      k,
					"filter": map[string]any{"bool": map[string]any{
						"filter": []any{map[string]any{"term": map[string]any{"kb_id": kbID}}},
					}},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("kb: marshal chunk search query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]RetrievedChunk, 0, len(hits))
	for _, h := range hits {
		var c Chunk
		if err := json.Unmarshal(h.Source, &c); err != nil {
			return nil, err
		}
		out = append(out, RetrievedChunk{Chunk: c, Score: h.Score})
	}
	return out, nil
}

// HasChunks reports whether a knowledge base contains any chunks. A cheap
// existence probe (no scoring, no source fetch) used to skip embedding a query
// for a KB with nothing in it.
func (r *ChunkRepository) HasChunks(ctx context.Context, kbID string) (bool, error) {
	if kbID == "" {
		return false, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":    1,
		"_source": false,
		"query":   map[string]any{"term": map[string]any{"kb_id": kbID}},
	})
	if err != nil {
		return false, fmt.Errorf("kb: marshal chunk exists query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return false, err
	}
	return len(hits) > 0, nil
}

// DeleteByKB removes every chunk of a knowledge base, for the delete cascade.
func (r *ChunkRepository) DeleteByKB(ctx context.Context, kbID string) error {
	if kbID == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"kb_id": kbID}},
	})
	if err != nil {
		return fmt.Errorf("kb: marshal chunk delete query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}
