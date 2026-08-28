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
	// ChunksIndex holds the embedded passages of retrieval knowledge bases.
	// The name is inherited from when a RAG collection belonged to an agent,
	// and is kept so an existing deployment's data stays where it is.
	ChunksIndex string `env:"OPENSEARCH_INDEX_AGENT_RAG_CHUNKS, default=enact-agent-rag-chunks"`
}

// KnowledgeBase is the metadata record for a knowledge base. The id is the
// identifier used across APIs; Name is the user-facing friendly name, given
// at creation and updatable.
type KnowledgeBase struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// OrganizationID is the organization this knowledge base belongs to. Stored rather
	// than inferred from the owner: every read compares it, and an owner
	// bypasses permission checks, so this is the only thing keeping one
	// organization out of another's data.
	OrganizationID string `json:"organization_id"`

	Name string `json:"name"`
	// Kind decides what an upload to this knowledge base becomes.
	//
	// A context KB stores each document WHOLE, and an agent referencing it
	// loads all of them into the model's context. A retrieval KB chunks and
	// embeds each document, and an agent attached to it gets only the passages
	// relevant to the question asked.
	//
	// Fixed once the KB holds documents: changing it would leave the existing
	// ones stored in a form nothing reads, which is worse than refusing.
	Kind string `json:"kind"`
	// ChunkSize and ChunkOverlap are how a retrieval KB splits its documents,
	// in runes. Absent (zero) on context knowledge bases, which store
	// documents whole, and on retrieval ones created before this was
	// settable — the indexer falls back to its configured default for those.
	//
	// Set at creation and never afterwards. Chunking happens at upload time,
	// so a value changed mid-life would apply only to documents added after
	// the change, leaving one knowledge base holding two incompatible
	// chunkings with nothing recording which is which. Same reasoning as
	// Kind above.
	//
	// Recorded concretely rather than left blank to mean "the default", so
	// that moving the platform default later cannot silently re-chunk what
	// somebody uploads to an existing knowledge base tomorrow.
	ChunkSize    int       `json:"chunk_size,omitempty"`
	ChunkOverlap int       `json:"chunk_overlap,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// The kinds a knowledge base can be.
const (
	// KindContext stores documents whole; every one is loaded into context.
	KindContext = "context"
	// KindRetrieval chunks and embeds documents; only the relevant passages
	// reach the model. An agent may attach exactly one.
	KindRetrieval = "rag"
)

// ValidKind reports whether a kind is one this platform knows.
func ValidKind(kind string) bool {
	return kind == KindContext || kind == KindRetrieval
}

// NormalizeKind fills in the default for a KB created before kinds existed,
// or by a caller that did not say.
//
// Context is the default because that is what every KB was before this: an
// existing record with no kind holds whole documents, and reading it as
// anything else would silently change what an agent using it receives.
func NormalizeKind(kind string) string {
	if kind == "" {
		return KindContext
	}
	return kind
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

// Get fetches a knowledge base by id, scoped to an organization.
//
// One belonging to a different organization is reported as ABSENT rather
// than refused: callers already render that as 404, and "not yours" must be
// indistinguishable from "does not exist". Scoping here rather than in the
// handlers matters because an organization owner passes every permission
// check by construction — see agents.Repository.Get.
func (r *Repository) Get(ctx context.Context, organizationID, id string) (KnowledgeBase, bool, error) {
	var kb KnowledgeBase
	found, err := r.os.GetSource(ctx, r.index, id, &kb)
	if err != nil || !found {
		return KnowledgeBase{}, found, err
	}
	if organizationID == "" || kb.OrganizationID != organizationID {
		return KnowledgeBase{}, false, nil
	}
	return kb, true, nil
}

// List returns the knowledge bases owned by any of userIDs — the caller's
// whole organization; see agents.Repository.List for why the filter widened.
func (r *Repository) List(ctx context.Context, organizationID string) ([]KnowledgeBase, error) {
	if organizationID == "" {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
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
