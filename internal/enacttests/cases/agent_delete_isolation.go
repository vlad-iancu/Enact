package cases

import (
	"enact/internal/enacttests/utils"
	"fmt"
	"net/http"
	"time"
)

// agentDeleteIsolationCase proves an agent deletion touches exactly the
// targeted record: the victim 404s and leaves the listing, while two
// bystanders created alongside it remain fetchable, unmutated, and listed.
type agentDeleteIsolationCase struct {
	victim     utils.AgentDTO
	bystander1 utils.AgentDTO
	bystander2 utils.AgentDTO
}

func NewAgentDeleteIsolation() utils.TestCase { return &agentDeleteIsolationCase{} }

func (c *agentDeleteIsolationCase) Name() string { return "TestAgentManagement_DeleteIsolation" }

// Setup creates the victim and both bystanders. An abort mid-way is safe:
// TearDown deletes whatever was created (empty ids are no-ops).
func (c *agentDeleteIsolationCase) Setup(t *utils.T) {
	c.victim = t.CreateAgent(`{"model":"claude-sonnet-4-6","system_prompt":"victim"}`)
	c.bystander1 = t.CreateAgent(`{"model":"claude-sonnet-4-6","system_prompt":"bystander one"}`)
	c.bystander2 = t.CreateAgent(`{"model":"claude-sonnet-4-6","system_prompt":"bystander two"}`)
}

func (c *agentDeleteIsolationCase) Run(t *utils.T) {
	t.DeleteAgent(c.victim.ID)

	if status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+c.victim.ID), nil, nil); status != http.StatusNotFound {
		t.Errorf("deleted agent still fetchable: got HTTP %d, want 404", status)
	}
	for _, b := range []utils.AgentDTO{c.bystander1, c.bystander2} {
		var fetched utils.AgentDTO
		if status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+b.ID), nil, &fetched); status != http.StatusOK {
			t.Errorf("bystander %s vanished with the deletion: got HTTP %d, want 200", b.ID, status)
			continue
		}
		if fetched.SystemPrompt != b.SystemPrompt {
			t.Errorf("bystander %s mutated: system_prompt = %q, want %q", b.ID, fetched.SystemPrompt, b.SystemPrompt)
		}
	}

	// Listings are search-backed and refresh asynchronously; poll until the
	// expected membership shows up. Membership is asserted by id, so
	// concurrently running cases cannot interfere.
	t.Eventually(5*time.Second, "listing reflects the deletion", func() (bool, string) {
		ids := t.ListAgentIDs()
		switch {
		case ids[c.victim.ID]:
			return false, fmt.Sprintf("deleted agent %s still present in listing", c.victim.ID)
		case !ids[c.bystander1.ID] || !ids[c.bystander2.ID]:
			return false, fmt.Sprintf("bystanders missing from listing: %s=%v %s=%v",
				c.bystander1.ID, ids[c.bystander1.ID], c.bystander2.ID, ids[c.bystander2.ID])
		default:
			return true, ""
		}
	})
}

// TearDown removes all three fixtures; the victim's delete is a tolerated
// 404 after a successful Run.
func (c *agentDeleteIsolationCase) TearDown(t *utils.T) {
	t.DeleteAgent(c.victim.ID)
	t.DeleteAgent(c.bystander1.ID)
	t.DeleteAgent(c.bystander2.ID)
}
