package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
)

// agentCreateGetDeleteCase verifies the basic agent lifecycle: a created
// agent is fetchable with the fields it was created with.
type agentCreateGetDeleteCase struct {
	utils.BaseCase
	agent utils.AgentDTO
}

func NewAgentCreateGetDelete() utils.TestCase { return &agentCreateGetDeleteCase{} }

func (c *agentCreateGetDeleteCase) Name() string { return "TestAgentManagement_CreateGetDelete" }

func (c *agentCreateGetDeleteCase) Run(t *utils.T) {
	c.agent = t.CreateAgent(`{"model":"claude-sonnet-4-6","system_prompt":"integration test agent"}`)

	var fetched utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/"+c.agent.ID), nil, &fetched)
	if status != http.StatusOK {
		t.Fatalf("get agent: got HTTP %d, want 200", status)
	}
	if fetched.Model != "claude-sonnet-4-6" {
		t.Errorf("get agent: model = %q, want claude-sonnet-4-6", fetched.Model)
	}
	if fetched.SystemPrompt != "integration test agent" {
		t.Errorf("get agent: system_prompt = %q", fetched.SystemPrompt)
	}
}

func (c *agentCreateGetDeleteCase) TearDown(t *utils.T) {
	t.DeleteAgent(c.agent.ID)
}
