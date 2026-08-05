package cases

import (
	"enact/internal/enacttests/utils"
	"net/http"
	"strings"
)

// agentRejectsUnknownModelCase verifies model validation on agent creation.
type agentRejectsUnknownModelCase struct {
	utils.BaseCase
}

func NewAgentRejectsUnknownModel() utils.TestCase { return &agentRejectsUnknownModelCase{} }

func (c *agentRejectsUnknownModelCase) Name() string {
	return "TestAgentManagement_RejectsUnknownModel"
}

func (c *agentRejectsUnknownModelCase) Run(t *utils.T) {
	var out utils.AgentDTO
	status := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPost, t.AgentURL("/v1/agents"),
		strings.NewReader(`{"model":"no-such-model"}`), &out)
	if status != http.StatusBadRequest {
		// Should never happen, but if the create went through, clean it up.
		t.DeleteAgent(out.ID)
		t.Fatalf("create with unknown model: got HTTP %d, want 400", status)
	}
	if !strings.Contains(out.Error, "no-such-model") {
		t.Errorf("error %q does not name the rejected model", out.Error)
	}
}
