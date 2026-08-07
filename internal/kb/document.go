package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/opensearch"
)

// Document is the extracted text of a file uploaded to a knowledge base.
// Documents are stored whole (not chunked or embedded): agents referencing
// the knowledge base load them into the model context at inference time.
type Document struct {
	UserID     string    `json:"user_id"`
	KBID       string    `json:"kb_id"`
	DocumentID string    `json:"document_id"`
	Filename   string    `json:"filename,omitempty"`
	Text       string    `json:"text"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// DocumentRepository persists the extracted documents of knowledge bases.
type DocumentRepository struct {
	os    *opensearch.Client
	index string
}

// NewDocumentRepository returns a DocumentRepository using the index in cfg.
func NewDocumentRepository(os *opensearch.Client, cfg Config) *DocumentRepository {
	return &DocumentRepository{os: os, index: cfg.DocumentsIndex}
}

// EnsureIndex verifies the KB documents index exists. The index and its
// mapping are owned by the composable index template in mappings/ and created
// by `make infrastructure-up`; this fails fast when it is missing.
func (r *DocumentRepository) EnsureIndex(ctx context.Context) error {
	exists, err := r.os.IndexExists(ctx, r.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("kb: required index %q is missing; run `make infrastructure-up` to create it", r.index)
	}
	return nil
}

// Index stores a document's extracted text. Re-indexing the same document id
// overwrites rather than duplicates.
func (r *DocumentRepository) Index(ctx context.Context, d Document) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, d.DocumentID, body)
}

// ListByKBs returns every document stored under the given knowledge bases,
// oldest first, restricted to the supplied user.
func (r *DocumentRepository) ListByKBs(ctx context.Context, userID string, kbIDs []string) ([]Document, error) {
	if len(kbIDs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size": 1000,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"user_id": userID}},
					map[string]any{"terms": map[string]any{"kb_id": kbIDs}},
				},
			},
		},
		"sort": []any{map[string]any{"uploaded_at": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("kb: marshal list documents query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(hits))
	for _, h := range hits {
		var d Document
		if err := json.Unmarshal(h.Source, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// DocumentMeta describes a stored document without its extracted text.
type DocumentMeta struct {
	DocumentID string    `json:"document_id"`
	Filename   string    `json:"filename,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// ListMetaByKB returns the metadata of every document stored under one
// knowledge base, oldest first, restricted to the supplied user. The
// (potentially large) extracted text is excluded at the OpenSearch level, so
// this stays cheap no matter the document sizes.
func (r *DocumentRepository) ListMetaByKB(ctx context.Context, userID, kbID string) ([]DocumentMeta, error) {
	body, err := json.Marshal(map[string]any{
		"size":    1000,
		"_source": map[string]any{"excludes": []string{"text"}},
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"user_id": userID}},
					map[string]any{"term": map[string]any{"kb_id": kbID}},
				},
			},
		},
		"sort": []any{map[string]any{"uploaded_at": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("kb: marshal list document meta query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]DocumentMeta, 0, len(hits))
	for _, h := range hits {
		var m DocumentMeta
		if err := json.Unmarshal(h.Source, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// DeleteByDocument removes one document, scoped to its knowledge base: the
// kb_id filter makes deleting another KB's document by guessed id
// structurally impossible. Idempotent — zero matches is success.
func (r *DocumentRepository) DeleteByDocument(ctx context.Context, kbID, documentID string) error {
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
		return fmt.Errorf("kb: marshal delete document query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}

// DeleteByKB removes every document stored under the given knowledge base.
func (r *DocumentRepository) DeleteByKB(ctx context.Context, kbID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"kb_id": kbID}},
	})
	if err != nil {
		return fmt.Errorf("kb: marshal delete documents query: %w", err)
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}
