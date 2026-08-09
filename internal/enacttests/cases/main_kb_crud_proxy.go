package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// mainKBCrudProxyCase verifies the knowledge-base surface through
// enact-main's session-guarded proxy: create with name, list, detail (with
// documents array), rename, and delete.
type mainKBCrudProxyCase struct {
	session *utils.MainSession
	kbID    string
}

func NewMainKBCrudProxy() utils.TestCase { return &mainKBCrudProxyCase{} }

func (c *mainKBCrudProxyCase) Name() string { return "TestMainKB_CrudProxy" }

func (c *mainKBCrudProxyCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)
}

func (c *mainKBCrudProxyCase) Run(t *utils.T) {
	s := c.session

	// Validation relay.
	if st := s.DoJSON(t, http.MethodPost, "/knowledge-bases", strings.NewReader(`{}`), nil); st != http.StatusBadRequest {
		t.Errorf("nameless create via proxy: got HTTP %d, want 400", st)
	}

	var created utils.KBDTO
	if st := s.DoJSON(t, http.MethodPost, "/knowledge-bases", strings.NewReader(`{"name":"proxy kb"}`), &created); st != http.StatusCreated {
		t.Fatalf("create via proxy: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.kbID = created.ID
	if created.Name != "proxy kb" {
		t.Errorf("create: name = %q, want %q", created.Name, "proxy kb")
	}

	var list struct {
		KnowledgeBases []utils.KBDTO `json:"knowledge_bases"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/knowledge-bases", nil, &list); st != http.StatusOK {
		t.Fatalf("list via proxy: got HTTP %d, want 200", st)
	}
	found := false
	for _, k := range list.KnowledgeBases {
		if k.ID == c.kbID {
			found = true
		}
	}
	if !found {
		t.Errorf("created kb missing from proxy listing (%d kbs)", len(list.KnowledgeBases))
	}

	// Detail carries the documents array (empty but present).
	var detailRaw map[string]any
	if st := s.DoJSON(t, http.MethodGet, "/knowledge-bases/"+c.kbID, nil, &detailRaw); st != http.StatusOK {
		t.Fatalf("detail via proxy: got HTTP %d, want 200", st)
	}
	if _, ok := detailRaw["documents"]; !ok {
		t.Errorf("detail response lacks the documents array")
	}

	var renamed utils.KBDTO
	if st := s.DoJSON(t, http.MethodPut, "/knowledge-bases/"+c.kbID, strings.NewReader(`{"name":"proxy kb renamed"}`), &renamed); st != http.StatusOK || renamed.Name != "proxy kb renamed" {
		t.Errorf("rename via proxy: HTTP %d name %q, want 200 %q", st, renamed.Name, "proxy kb renamed")
	}

	if st := s.DoJSON(t, http.MethodDelete, "/knowledge-bases/"+c.kbID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete via proxy: got HTTP %d, want 204", st)
	}
	deleted := c.kbID
	c.kbID = ""
	if st := s.DoJSON(t, http.MethodGet, "/knowledge-bases/"+deleted, nil, nil); st != http.StatusNotFound {
		t.Errorf("detail after delete: got HTTP %d, want 404", st)
	}
}

func (c *mainKBCrudProxyCase) TearDown(t *utils.T) {
	if c.kbID != "" && c.session != nil {
		if st := c.session.DoJSON(t, http.MethodDelete, "/knowledge-bases/"+c.kbID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
			t.Errorf("cleanup kb %s: got HTTP %d", c.kbID, st)
		}
	}
}
