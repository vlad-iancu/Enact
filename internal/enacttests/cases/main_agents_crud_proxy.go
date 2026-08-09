package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// mainAgentsCrudProxyCase verifies the full agent management surface through
// enact-main's session-guarded proxy: create (with validation relay), get,
// list, partial update, and delete.
type mainAgentsCrudProxyCase struct {
	session *utils.MainSession
	agentID string
}

func NewMainAgentsCrudProxy() utils.TestCase { return &mainAgentsCrudProxyCase{} }

func (c *mainAgentsCrudProxyCase) Name() string { return "TestMainAgents_CrudProxy" }

func (c *mainAgentsCrudProxyCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)
}

func (c *mainAgentsCrudProxyCase) Run(t *utils.T) {
	s := c.session

	// Downstream validation relays through the proxy as a 400.
	var errOut struct {
		Error string `json:"error"`
	}
	if st := s.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(`{"model":"claude-sonnet-4-6"}`), &errOut); st != http.StatusBadRequest {
		t.Fatalf("nameless create via proxy: got HTTP %d, want 400", st)
	}
	if !strings.Contains(errOut.Error, "name") {
		t.Errorf("relayed error %q does not mention the name", errOut.Error)
	}

	var created utils.AgentDTO
	if st := s.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(`{"name":"proxy agent","model":"claude-sonnet-4-6","system_prompt":"via proxy"}`), &created); st != http.StatusCreated {
		t.Fatalf("create via proxy: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.agentID = created.ID
	t.Logf("created agent %s via proxy", c.agentID)

	var fetched utils.AgentDTO
	if st := s.DoJSON(t, http.MethodGet, "/agents/"+c.agentID, nil, &fetched); st != http.StatusOK || fetched.Name != "proxy agent" {
		t.Errorf("get via proxy: HTTP %d name %q, want 200 %q", st, fetched.Name, "proxy agent")
	}

	var list struct {
		Agents []utils.AgentDTO `json:"agents"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/agents", nil, &list); st != http.StatusOK {
		t.Fatalf("list via proxy: got HTTP %d, want 200", st)
	}
	found := false
	for _, a := range list.Agents {
		if a.ID == c.agentID {
			found = true
		}
	}
	if !found {
		t.Errorf("created agent missing from proxy listing (%d agents)", len(list.Agents))
	}

	var updated utils.AgentDTO
	if st := s.DoJSON(t, http.MethodPut, "/agents/"+c.agentID, strings.NewReader(`{"system_prompt":"updated"}`), &updated); st != http.StatusOK {
		t.Fatalf("update via proxy: got HTTP %d, want 200", st)
	}
	if updated.SystemPrompt != "updated" || updated.Name != "proxy agent" {
		t.Errorf("partial update via proxy: prompt %q name %q, want %q %q", updated.SystemPrompt, updated.Name, "updated", "proxy agent")
	}

	if st := s.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete via proxy: got HTTP %d, want 204", st)
	}
	deleted := c.agentID
	c.agentID = ""
	if st := s.DoJSON(t, http.MethodGet, "/agents/"+deleted, nil, nil); st != http.StatusNotFound {
		t.Errorf("get after delete: got HTTP %d, want 404", st)
	}
}

func (c *mainAgentsCrudProxyCase) TearDown(t *utils.T) {
	if c.agentID != "" && c.session != nil {
		if st := c.session.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
			t.Errorf("cleanup agent %s: got HTTP %d", c.agentID, st)
		}
	}
}
