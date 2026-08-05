package utils

import (
	"net/http"
)

// Helpers for the knowledge-base cases. kb-api is internal (no anonymous
// access), so these double as an exercise of the impersonation fleet:
// requests are signed as enact-tests.

type KBDTO struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Error     string `json:"error"`
	Documents []struct {
		DocumentID string `json:"document_id"`
		Filename   string `json:"filename"`
	} `json:"documents"`
}

const KBAudience = "enact-kb-api"

func (t *T) KBURL(path string) string { return t.Env.KBAPIURL + path }

// CreateKB creates a throwaway knowledge base; pair every call with a
// DeleteKB in the case's TearDown.
func (t *T) CreateKB() KBDTO {
	var out KBDTO
	status := t.DoJSON("enact-tests", KBAudience, http.MethodPost, t.KBURL("/v1/knowledge-bases"), nil, &out)
	if status != http.StatusCreated {
		t.Fatalf("create kb: got HTTP %d (%s), want 201", status, out.Error)
	}
	if out.ID == "" {
		t.Fatalf("create kb: response has no id")
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
