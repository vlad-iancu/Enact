package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// kbNameLifecycleCase verifies the KB friendly name: required at creation,
// returned everywhere, and updatable via PUT without touching anything else.
type kbNameLifecycleCase struct {
	kb utils.KBDTO
}

func NewKBNameLifecycle() utils.TestCase { return &kbNameLifecycleCase{} }

func (c *kbNameLifecycleCase) Name() string { return "TestKB_NameLifecycle" }

func (c *kbNameLifecycleCase) Setup(t *utils.T) {
	c.kb = t.CreateKB()
}

func (c *kbNameLifecycleCase) Run(t *utils.T) {
	if c.kb.Name != "integration test kb" {
		t.Errorf("create: name = %q, want %q", c.kb.Name, "integration test kb")
	}

	// Nameless creation is rejected.
	var errOut utils.KBDTO
	status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPost, t.KBURL("/v1/knowledge-bases"),
		strings.NewReader(`{}`), &errOut)
	if status != http.StatusBadRequest {
		if errOut.ID != "" {
			t.DeleteKB(errOut.ID)
		}
		t.Fatalf("nameless create: got HTTP %d, want 400", status)
	}

	// Rename via PUT.
	var renamed utils.KBDTO
	status = t.DoJSON("enact-tests", utils.KBAudience, http.MethodPut, t.KBURL("/v1/knowledge-bases/"+c.kb.ID),
		strings.NewReader(`{"name":"renamed kb"}`), &renamed)
	if status != http.StatusOK {
		t.Fatalf("rename: got HTTP %d (%s), want 200", status, renamed.Error)
	}
	if renamed.Name != "renamed kb" {
		t.Errorf("rename: name = %q, want %q", renamed.Name, "renamed kb")
	}

	// Blank rename is rejected; empty body keeps the name.
	if status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPut, t.KBURL("/v1/knowledge-bases/"+c.kb.ID),
		strings.NewReader(`{"name":"  "}`), nil); status != http.StatusBadRequest {
		t.Errorf("blank rename: got HTTP %d, want 400", status)
	}
	var kept utils.KBDTO
	if status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPut, t.KBURL("/v1/knowledge-bases/"+c.kb.ID),
		strings.NewReader(`{}`), &kept); status != http.StatusOK || kept.Name != "renamed kb" {
		t.Errorf("empty-body update: got HTTP %d name %q, want 200 %q", status, kept.Name, "renamed kb")
	}

	// The new name shows on GET.
	var fetched utils.KBDTO
	if status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &fetched); status != http.StatusOK || fetched.Name != "renamed kb" {
		t.Errorf("get after rename: got HTTP %d name %q, want 200 %q", status, fetched.Name, "renamed kb")
	}
}

func (c *kbNameLifecycleCase) TearDown(t *utils.T) {
	t.DeleteKB(c.kb.ID)
}
