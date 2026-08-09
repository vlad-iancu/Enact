package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// agentNameLifecycleCase verifies the agent friendly name: required at
// creation, updatable alone, and preserved by unrelated partial updates.
type agentNameLifecycleCase struct {
	agent utils.AgentDTO
}

func NewAgentNameLifecycle() utils.TestCase { return &agentNameLifecycleCase{} }

func (c *agentNameLifecycleCase) Name() string { return "TestAgentManagement_NameLifecycle" }

func (c *agentNameLifecycleCase) Setup(t *utils.T) {
	c.agent = t.CreateAgent(`{"name":"integration test agent","model":"claude-sonnet-4-6","system_prompt":"original"}`)
}

func (c *agentNameLifecycleCase) Run(t *utils.T) {
	if c.agent.Name != "integration test agent" {
		t.Errorf("create: name = %q, want %q", c.agent.Name, "integration test agent")
	}

	// Nameless creation is rejected, and the message names the gap.
	var errOut utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPost, t.AgentURL("/v1/agents"),
		strings.NewReader(`{"model":"claude-sonnet-4-6"}`), &errOut)
	if status != http.StatusBadRequest {
		if errOut.ID != "" {
			t.DeleteAgent(errOut.ID)
		}
		t.Fatalf("nameless create: got HTTP %d, want 400", status)
	}
	if !strings.Contains(errOut.Error, "name") {
		t.Errorf("nameless create error %q does not mention the name", errOut.Error)
	}

	// Rename alone.
	var renamed utils.AgentDTO
	status = t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPut, t.AgentURL("/v1/agents/"+c.agent.ID),
		strings.NewReader(`{"name":"renamed agent"}`), &renamed)
	if status != http.StatusOK {
		t.Fatalf("rename: got HTTP %d (%s), want 200", status, renamed.Error)
	}
	if renamed.Name != "renamed agent" || renamed.SystemPrompt != "original" {
		t.Errorf("rename: name %q prompt %q, want %q %q", renamed.Name, renamed.SystemPrompt, "renamed agent", "original")
	}

	// A prompt-only update keeps the name.
	var updated utils.AgentDTO
	if status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPut, t.AgentURL("/v1/agents/"+c.agent.ID),
		strings.NewReader(`{"system_prompt":"changed"}`), &updated); status != http.StatusOK || updated.Name != "renamed agent" {
		t.Errorf("prompt-only update: got HTTP %d name %q, want 200 %q", status, updated.Name, "renamed agent")
	}

	// Blank rename is rejected.
	if status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPut, t.AgentURL("/v1/agents/"+c.agent.ID),
		strings.NewReader(`{"name":" "}`), nil); status != http.StatusBadRequest {
		t.Errorf("blank rename: got HTTP %d, want 400", status)
	}
}

func (c *agentNameLifecycleCase) TearDown(t *utils.T) {
	t.DeleteAgent(c.agent.ID)
}
