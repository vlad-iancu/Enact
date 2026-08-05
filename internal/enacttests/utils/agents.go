package utils

import (
	"net/http"
	"strings"
)

// Helpers for the agent-management cases. All calls impersonate the
// enact-tests identity; the agent API admits any authenticated service.

type AgentDTO struct {
	ID               string   `json:"id"`
	Model            string   `json:"model"`
	SystemPrompt     string   `json:"system_prompt"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	Error            string   `json:"error"`
}

const AgentAudience = "enact-agent-management-api"

func (t *T) AgentURL(path string) string { return t.Env.AgentAPIURL + path }

// CreateAgent creates a throwaway agent; pair every call with a DeleteAgent
// in the case's TearDown.
func (t *T) CreateAgent(body string) AgentDTO {
	var out AgentDTO
	status := t.DoJSON("enact-tests", AgentAudience, http.MethodPost, t.AgentURL("/v1/agents"), strings.NewReader(body), &out)
	if status != http.StatusCreated {
		t.Fatalf("create agent: got HTTP %d (%s), want 201", status, out.Error)
	}
	if out.ID == "" {
		t.Fatalf("create agent: response has no id")
	}
	return out
}

// DeleteAgent removes an agent. It is TearDown-tolerant: an empty id
// (fixture never created) is a no-op and 404 (already deleted) is accepted;
// any other failure marks the case failed.
func (t *T) DeleteAgent(id string) {
	if id == "" {
		return
	}
	status := t.DoJSON("enact-tests", AgentAudience, http.MethodDelete, t.AgentURL("/v1/agents/"+id), nil, nil)
	if status != http.StatusNoContent && status != http.StatusNotFound {
		t.Errorf("delete agent %s: got HTTP %d, want 204", id, status)
	}
}

// ListAgentIDs returns the ids of the caller's agents.
func (t *T) ListAgentIDs() map[string]bool {
	var out struct {
		Agents []AgentDTO `json:"agents"`
		Error  string     `json:"error"`
	}
	status := t.DoJSON("enact-tests", AgentAudience, http.MethodGet, t.AgentURL("/v1/agents"), nil, &out)
	if status != http.StatusOK {
		t.Fatalf("list agents: got HTTP %d (%s), want 200", status, out.Error)
	}
	ids := make(map[string]bool, len(out.Agents))
	for _, a := range out.Agents {
		ids[a.ID] = true
	}
	return ids
}
