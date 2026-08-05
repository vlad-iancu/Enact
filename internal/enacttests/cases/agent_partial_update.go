package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
	"strings"
)

// agentPartialUpdateCase verifies PUT's partial-update semantics: a provided
// field changes, an absent one keeps its value.
type agentPartialUpdateCase struct {
	agent utils.AgentDTO
}

func NewAgentPartialUpdate() utils.TestCase { return &agentPartialUpdateCase{} }

func (c *agentPartialUpdateCase) Name() string { return "TestAgentManagement_PartialUpdate" }

func (c *agentPartialUpdateCase) Setup(t *utils.T) {
	c.agent = t.CreateAgent(`{"model":"claude-sonnet-4-6","system_prompt":"original"}`)
}

func (c *agentPartialUpdateCase) Run(t *utils.T) {
	// Update only the prompt; the model must survive.
	var updated utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPut, t.AgentURL("/v1/agents/"+c.agent.ID),
		strings.NewReader(`{"system_prompt":"changed"}`), &updated)
	if status != http.StatusOK {
		t.Fatalf("partial update: got HTTP %d (%s), want 200", status, updated.Error)
	}
	if updated.SystemPrompt != "changed" {
		t.Errorf("partial update: system_prompt = %q, want %q", updated.SystemPrompt, "changed")
	}
	if updated.Model != "claude-sonnet-4-6" {
		t.Errorf("partial update: model = %q, want unchanged claude-sonnet-4-6", updated.Model)
	}
}

func (c *agentPartialUpdateCase) TearDown(t *utils.T) {
	t.DeleteAgent(c.agent.ID)
}
