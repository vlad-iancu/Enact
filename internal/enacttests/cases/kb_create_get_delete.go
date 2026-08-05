package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
)

// kbCreateGetDeleteCase verifies the basic knowledge-base lifecycle and that
// ownership follows the impersonated test user.
type kbCreateGetDeleteCase struct {
	utils.BaseCase
	kb utils.KBDTO
}

func NewKBCreateGetDelete() utils.TestCase { return &kbCreateGetDeleteCase{} }

func (c *kbCreateGetDeleteCase) Name() string { return "TestKB_CreateGetDelete" }

func (c *kbCreateGetDeleteCase) Run(t *utils.T) {
	c.kb = t.CreateKB()

	if c.kb.UserID != t.Env.UserID {
		t.Errorf("create kb: user_id = %q, want %q (identity header not honoured?)", c.kb.UserID, t.Env.UserID)
	}

	var fetched utils.KBDTO
	status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &fetched)
	if status != http.StatusOK {
		t.Fatalf("get kb: got HTTP %d, want 200", status)
	}
	if fetched.ID != c.kb.ID {
		t.Errorf("get kb: id = %q, want %q", fetched.ID, c.kb.ID)
	}
}

func (c *kbCreateGetDeleteCase) TearDown(t *utils.T) {
	t.DeleteKB(c.kb.ID)
}
