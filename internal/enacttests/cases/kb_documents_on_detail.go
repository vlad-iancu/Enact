package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
)

// kbDocumentsOnDetailCase verifies the KB detail response carries a (present
// but empty) documents list for a fresh knowledge base.
type kbDocumentsOnDetailCase struct {
	kb utils.KBDTO
}

func NewKBDocumentsOnDetail() utils.TestCase { return &kbDocumentsOnDetailCase{} }

func (c *kbDocumentsOnDetailCase) Name() string { return "TestKB_DocumentsListedOnDetail" }

func (c *kbDocumentsOnDetailCase) Setup(t *utils.T) {
	c.kb = t.CreateKB()
}

func (c *kbDocumentsOnDetailCase) Run(t *utils.T) {
	var fetched utils.KBDTO
	status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.kb.ID), nil, &fetched)
	if status != http.StatusOK {
		t.Fatalf("get kb: got HTTP %d, want 200", status)
	}
	if len(fetched.Documents) != 0 {
		t.Errorf("fresh kb reports %d documents, want 0", len(fetched.Documents))
	}
}

func (c *kbDocumentsOnDetailCase) TearDown(t *utils.T) {
	t.DeleteKB(c.kb.ID)
}
