package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
)

// agentGetMissingCase verifies unknown agent ids yield 404.
type agentGetMissingCase struct {
	utils.BaseCase
}

func NewAgentGetMissing() utils.TestCase { return &agentGetMissingCase{} }

func (c *agentGetMissingCase) Name() string { return "TestAgentManagement_GetMissingIs404" }

func (c *agentGetMissingCase) Run(t *utils.T) {
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet, t.AgentURL("/v1/agents/no-such-agent-id"), nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("get missing agent: got HTTP %d, want 404", status)
	}
}
