package cases

import (
	"enact/internal/enacttests/utils"
	"fmt"
	"net/http"
	"time"
)

// kbDeleteIsolationCase proves a knowledge-base deletion touches exactly the
// targeted record: the victim 404s and leaves the listing, while two
// bystanders created alongside it remain fetchable and listed.
type kbDeleteIsolationCase struct {
	victim     utils.KBDTO
	bystander1 utils.KBDTO
	bystander2 utils.KBDTO
}

func NewKBDeleteIsolation() utils.TestCase { return &kbDeleteIsolationCase{} }

func (c *kbDeleteIsolationCase) Name() string { return "TestKB_DeleteIsolation" }

// Setup creates the victim and both bystanders. An abort mid-way is safe:
// TearDown deletes whatever was created (empty ids are no-ops).
func (c *kbDeleteIsolationCase) Setup(t *utils.T) {
	c.victim = t.CreateKB()
	c.bystander1 = t.CreateKB()
	c.bystander2 = t.CreateKB()
}

func (c *kbDeleteIsolationCase) Run(t *utils.T) {
	t.DeleteKB(c.victim.ID)

	if status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+c.victim.ID), nil, nil); status != http.StatusNotFound {
		t.Errorf("deleted kb still fetchable: got HTTP %d, want 404", status)
	}
	for _, b := range []utils.KBDTO{c.bystander1, c.bystander2} {
		if status := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet, t.KBURL("/v1/knowledge-bases/"+b.ID), nil, nil); status != http.StatusOK {
			t.Errorf("bystander kb %s vanished with the deletion: got HTTP %d, want 200", b.ID, status)
		}
	}

	// Listings are search-backed and refresh asynchronously; poll until the
	// expected membership shows up. Membership is asserted by id, so
	// concurrently running cases cannot interfere.
	t.Eventually(5*time.Second, "listing reflects the deletion", func() (bool, string) {
		ids := t.ListKBIDs()
		switch {
		case ids[c.victim.ID]:
			return false, fmt.Sprintf("deleted kb %s still present in listing", c.victim.ID)
		case !ids[c.bystander1.ID] || !ids[c.bystander2.ID]:
			return false, fmt.Sprintf("bystander kbs missing from listing: %s=%v %s=%v",
				c.bystander1.ID, ids[c.bystander1.ID], c.bystander2.ID, ids[c.bystander2.ID])
		default:
			return true, ""
		}
	})
}

// TearDown removes all three fixtures; the victim's delete is a tolerated
// 404 after a successful Run.
func (c *kbDeleteIsolationCase) TearDown(t *utils.T) {
	t.DeleteKB(c.victim.ID)
	t.DeleteKB(c.bystander1.ID)
	t.DeleteKB(c.bystander2.ID)
}
