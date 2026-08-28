package utils

import (
	"net/http"
	"strings"
)

// Helpers for the knowledge-base cases. kb-api is internal (no anonymous
// access), so these double as an exercise of the impersonation fleet:
// requests are signed as enact-tests.

type KBDTO struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	// ChunkSize and ChunkOverlap are present only on retrieval knowledge
	// bases; a context one reports neither.
	ChunkSize    int    `json:"chunk_size"`
	ChunkOverlap int    `json:"chunk_overlap"`
	Error        string `json:"error"`
	Documents    []struct {
		DocumentID string `json:"document_id"`
		Filename   string `json:"filename"`
		Chunks     int    `json:"chunks"`
	} `json:"documents"`
}

const KBAudience = "enact-kb-api"

func (t *T) KBURL(path string) string { return t.Env.KBAPIURL + path }

// CreateKB creates a throwaway context knowledge base; pair every call with a
// DeleteKB in the case's TearDown.
func (t *T) CreateKB() KBDTO { return t.CreateKBOfKind("context") }

// CreateKBOfKind creates a throwaway knowledge base of the given kind
// ("context" or "rag"); pair every call with a DeleteKB in TearDown.
func (t *T) CreateKBOfKind(kind string) KBDTO {
	var out KBDTO
	// The name is the same for both kinds: cases assert on it, and it names
	// the fixture, not its storage strategy.
	body := `{"name":"integration test kb","kind":"` + kind + `"}`
	status := t.DoJSON("enact-tests", KBAudience, http.MethodPost, t.KBURL("/v1/knowledge-bases"),
		strings.NewReader(body), &out)
	if status != http.StatusCreated {
		t.Fatalf("create %s kb: got HTTP %d (%s), want 201", kind, status, out.Error)
	}
	if out.ID == "" {
		t.Fatalf("create %s kb: response has no id", kind)
	}
	if out.Kind != kind {
		t.Fatalf("create %s kb: response reports kind %q", kind, out.Kind)
	}
	return out
}

// DeleteKB removes a knowledge base. It is TearDown-tolerant: an empty id
// (fixture never created) is a no-op and 404 (already deleted) is accepted;
// any other failure marks the case failed.
func (t *T) DeleteKB(id string) {
	if id == "" {
		return
	}
	status := t.DoJSON("enact-tests", KBAudience, http.MethodDelete, t.KBURL("/v1/knowledge-bases/"+id), nil, nil)
	if status != http.StatusNoContent && status != http.StatusNotFound {
		t.Errorf("delete kb %s: got HTTP %d, want 204", id, status)
	}
}

// ListKBIDs returns the ids of the caller's knowledge bases.
func (t *T) ListKBIDs() map[string]bool {
	var out struct {
		KnowledgeBases []KBDTO `json:"knowledge_bases"`
		Error          string  `json:"error"`
	}
	status := t.DoJSON("enact-tests", KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases"), nil, &out)
	if status != http.StatusOK {
		t.Fatalf("list kbs: got HTTP %d (%s), want 200", status, out.Error)
	}
	ids := make(map[string]bool, len(out.KnowledgeBases))
	for _, k := range out.KnowledgeBases {
		ids[k.ID] = true
	}
	return ids
}
