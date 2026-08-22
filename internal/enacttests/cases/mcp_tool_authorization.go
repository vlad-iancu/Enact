package cases

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"enact/internal/enacttests/utils"
)

const (
	waitProviderName = "e2e-wait-cred"
	waitServerID     = "e2e-wait-authed"
)

// mcpToolAuthorizationCase proves what happens when a tool needs a
// credential the user has not connected: the call is REFUSED, immediately,
// naming what to connect — it does not wait for anything.
//
// It then connects the account and asks again, which must succeed. Two
// requests, not one stream: nothing resumes a refused call, and the second
// request is how a user retries.
//
// It runs through enact-main, so it also covers the SSE relay (whose default
// branch silently drops unknown event names).
type mcpToolAuthorizationCase struct {
	utils.BaseCase
	session *utils.MainSession
	agentID string
}

func NewMCPToolAuthorization() utils.TestCase { return &mcpToolAuthorizationCase{} }

func (c *mcpToolAuthorizationCase) Name() string { return "TestMCPTools_RefusesWithoutAuthorization" }

func (c *mcpToolAuthorizationCase) Setup(t *utils.T) {
	c.TearDown(t)

	provider := fmt.Sprintf(`{"name":%q,"display_name":"E2E wait credential","scheme":"bearer","access_levels":{"read":["fixture:read"]}}`, waitProviderName)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		t.Env.IdentitiesURL+"/v1/providers/pat", strings.NewReader(provider), nil); st != http.StatusCreated {
		t.Fatalf("register credential provider: got HTTP %d, want 201", st)
	}

	server := fmt.Sprintf(`{
      "id": %q,
      "url": %q,
      "transport_type": "streamable-http",
      "description": "authorization wait fixture",
      "tool_access_requirements": {"secret_number_authed": [{"provider": %q, "access_level": "read"}]},
      "tool_authorizations": {"secret_number_authed": {"headers_authorization": [{"header_name": %q, "header_template": "Bearer {{ (cred \"%s\").Credentials }}"}]}}
    }`, waitServerID, t.Env.MCPFixtureURL, waitProviderName, utils.FixtureAuthHeader, waitProviderName)
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(server), nil); st != http.StatusCreated {
		t.Fatalf("register mcp server: got HTTP %d, want 201", st)
	}

	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E MCP Wait", mainTestEmail, mainTestPassword)

	agent := fmt.Sprintf(`{"name":"e2e mcp wait agent","model":"claude-haiku-4-5","system_prompt":"Use the tools you are given.","tools":[%q]}`, waitServerID)
	var created struct {
		ID string `json:"id"`
	}
	if st := c.session.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(agent), &created); st != http.StatusCreated {
		t.Fatalf("create agent: got HTTP %d, want 201", st)
	}
	c.agentID = created.ID

	// The credential must NOT exist when the run starts; a leftover from an
	// aborted run would make the refusal never happen.
	c.session.DoJSON(t, http.MethodDelete, "/identities/"+waitProviderName, nil, nil)
}

// requirements asks enact-main what the session user still has to connect
// for this agent.
func (c *mcpToolAuthorizationCase) requirements(t *utils.T) (satisfied bool, reason string, levels int) {
	var out struct {
		Requirements []struct {
			Servers      []string            `json:"servers"`
			Tools        []string            `json:"tools"`
			Provider     string              `json:"provider"`
			AccessLevel  string              `json:"access_level"`
			Connected    bool                `json:"connected"`
			Reason       string              `json:"reason"`
			ProviderType string              `json:"provider_type"`
			AccessLevels map[string][]string `json:"access_levels"`
		} `json:"requirements"`
		Satisfied bool `json:"satisfied"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/agents/"+c.agentID+"/required-identities", nil, &out); st != http.StatusOK {
		t.Fatalf("required-identities: got HTTP %d, want 200", st)
	}
	if len(out.Requirements) != 1 {
		t.Fatalf("required-identities returned %d requirements, want 1", len(out.Requirements))
	}
	r := out.Requirements[0]
	if len(r.Servers) != 1 || r.Servers[0] != waitServerID || r.Provider != waitProviderName || r.AccessLevel != "read" {
		t.Errorf("requirement = %+v, want the fixture server and provider at level read", r)
	}
	if len(r.Tools) != 1 || r.Tools[0] != "secret_number_authed" {
		t.Errorf("requirement tools = %v, want [secret_number_authed]", r.Tools)
	}
	// The UI needs to know HOW to connect without another round trip.
	if r.ProviderType != "pat" {
		t.Errorf("requirement provider_type = %q, want pat", r.ProviderType)
	}
	return out.Satisfied, r.Reason, len(r.AccessLevels)
}

func (c *mcpToolAuthorizationCase) Run(t *utils.T) {
	// Before connecting, the query and the runtime must agree: the agent
	// needs something the user does not have.
	if satisfied, reason, levels := c.requirements(t); satisfied || reason != "not_connected" || levels == 0 {
		t.Errorf("before connecting: satisfied=%v reason=%q access_levels=%d, want unsatisfied/not_connected with levels",
			satisfied, reason, levels)
	}

	// First attempt: refused, and the refusal says what to connect.
	refusal, result, announced := c.ask(t)
	if refusal.ToolUseID == "" {
		t.Fatalf("no toolCallAuthorizationRequired event arrived; the tool ran without a credential")
	}
	if !announced[refusal.ToolUseID] {
		t.Errorf("toolCallAuthorizationRequired arrived before toolCall for %s", refusal.ToolUseID)
	}
	if refusal.ServerID != waitServerID {
		t.Errorf("refusal server_id = %q, want %q", refusal.ServerID, waitServerID)
	}
	if len(refusal.Missing) != 1 || refusal.Missing[0].Provider != waitProviderName {
		t.Errorf("refusal missing = %+v, want the provider named", refusal.Missing)
	} else if refusal.Missing[0].Reason != "not_connected" || refusal.Missing[0].AccessLevel != "read" {
		t.Errorf("refusal missing[0] = %+v, want reason not_connected at level read", refusal.Missing[0])
	}
	// The model is told too, as a failed tool result — not left waiting.
	if !result.IsError {
		t.Errorf("tool result is_error = false, want true for a refused call")
	}
	if !strings.Contains(result.Content, waitProviderName) {
		t.Errorf("tool result %q does not name the provider to connect", result.Content)
	}

	// Connect the account, then ask again. Nothing resumed the first call;
	// this is a fresh request, which is what a user would do.
	connect := fmt.Sprintf(`{"provider":%q,"token":%q,"access_level":"read"}`,
		waitProviderName, utils.FixtureMCPToken)
	if st := c.session.DoJSON(t, http.MethodPost, "/identities/pat", strings.NewReader(connect), nil); st != http.StatusCreated {
		t.Fatalf("connect the account: got HTTP %d, want 201", st)
	}
	if satisfied, _, _ := c.requirements(t); !satisfied {
		t.Errorf("after connecting, required-identities still reports unsatisfied")
	}

	refusal, result, _ = c.ask(t)
	if refusal.ToolUseID != "" {
		t.Errorf("still refused after connecting: %+v", refusal.Missing)
	}
	if result.IsError {
		t.Errorf("tool result after connecting is an error: %q", result.Content)
	}
	if !strings.Contains(result.Content, utils.SecretNumber) {
		t.Errorf("tool result = %q, want the fixture's secret number", result.Content)
	}
}

// ask runs one inference and returns the authorization refusal (zero if
// none), the tool result, and which calls were announced.
func (c *mcpToolAuthorizationCase) ask(t *utils.T) (refusalEvent, resultEvent, map[string]bool) {
	var (
		mu        sync.Mutex
		refusal   refusalEvent
		result    resultEvent
		announced = map[string]bool{}
	)
	body := fmt.Sprintf(`{"agent_id":%q,"messages":[{"role":"user","content":"Call the secret_number_authed tool and reply with only the number it returns."}]}`, c.agentID)
	c.session.StreamSSE(t, "/inference", strings.NewReader(body), func(event, data string) bool {
		switch event {
		case "toolCall":
			var ev struct {
				ToolUseID string `json:"tool_use_id"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Errorf("tool call event is not valid JSON: %v", err)
				return false
			}
			mu.Lock()
			announced[ev.ToolUseID] = true
			mu.Unlock()
		case "toolCallAuthorizationRequired":
			var ev refusalEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Errorf("authorization event is not valid JSON: %v", err)
				return false
			}
			mu.Lock()
			refusal = ev
			mu.Unlock()
		case "toolCallResult":
			var ev resultEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Errorf("tool result is not valid JSON: %v", err)
				return false
			}
			mu.Lock()
			result = ev
			mu.Unlock()
			return false // the call settled; stop reading
		}
		return true
	})
	return refusal, result, announced
}

type refusalEvent struct {
	ServerID  string `json:"server_id"`
	Tool      string `json:"tool"`
	ToolUseID string `json:"tool_use_id"`
	Missing   []struct {
		Provider    string `json:"provider"`
		AccessLevel string `json:"access_level"`
		Reason      string `json:"reason"`
	} `json:"missing"`
}

type resultEvent struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

func (c *mcpToolAuthorizationCase) TearDown(t *utils.T) {
	if c.session != nil {
		if c.agentID != "" {
			c.session.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil)
		}
		c.session.DoJSON(t, http.MethodDelete, "/identities/"+waitProviderName, nil, nil)
	}
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id="+waitServerID, nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/providers/"+waitProviderName, nil, nil)
}
