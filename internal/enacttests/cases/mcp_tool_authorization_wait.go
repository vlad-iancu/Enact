package cases

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"enact/internal/enacttests/utils"
)

const (
	waitProviderName = "e2e-wait-cred"
	waitServerID     = "e2e-wait-authed"
)

// mcpToolAuthorizationWaitCase proves the whole waiting protocol, driven
// exactly as a browser would drive it: a tool call whose credential is
// missing announces toolCallWaitingAuthorization, the user connects the
// account WHILE the stream stays open, and the same call then succeeds.
//
// It runs through enact-main, so it also covers the SSE relay (whose
// default branch silently drops unknown event names).
type mcpToolAuthorizationWaitCase struct {
	utils.BaseCase
	session *utils.MainSession
	agentID string
}

func NewMCPToolAuthorizationWait() utils.TestCase { return &mcpToolAuthorizationWaitCase{} }

func (c *mcpToolAuthorizationWaitCase) Name() string { return "TestMCPTools_WaitsForAuthorization" }

func (c *mcpToolAuthorizationWaitCase) Setup(t *utils.T) {
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

	// The credential must NOT exist when the run starts; the session user's
	// leftovers from an aborted run would make the wait never happen.
	c.session.DoJSON(t, http.MethodDelete, "/identities/"+waitProviderName, nil, nil)
}

// requirements asks enact-main what the session user still has to connect
// for this agent.
func (c *mcpToolAuthorizationWaitCase) requirements(t *utils.T) (satisfied bool, reason string, levels int) {
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

func (c *mcpToolAuthorizationWaitCase) Run(t *utils.T) {
	// Before connecting, the query and the runtime must agree: the agent
	// needs something the user does not have.
	if satisfied, reason, levels := c.requirements(t); satisfied || reason != "not_connected" || levels == 0 {
		t.Errorf("before connecting: satisfied=%v reason=%q access_levels=%d, want unsatisfied/not_connected with levels",
			satisfied, reason, levels)
	}

	type waitEvent struct {
		ServerID  string `json:"server_id"`
		Tool      string `json:"tool"`
		ToolUseID string `json:"tool_use_id"`
		Status    string `json:"status"`
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

	var (
		mu           sync.Mutex
		sawWaiting   bool
		connectedAt  time.Time
		resolvedSeen bool
		result       resultEvent
		waitToolUse  string
		connectOnce  sync.Once
	)

	body := fmt.Sprintf(`{"agent_id":%q,"messages":[{"role":"user","content":"Call the secret_number_authed tool and reply with only the number it returns."}]}`, c.agentID)
	c.session.StreamSSE(t, "/inference", strings.NewReader(body), func(event, data string) bool {
		switch event {
		case "toolCallWaitingAuthorization":
			var ev waitEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Errorf("waiting event is not valid JSON: %v", err)
				return false
			}
			if ev.Status == "waiting" {
				mu.Lock()
				sawWaiting = true
				waitToolUse = ev.ToolUseID
				mu.Unlock()

				// The payload must tell the UI exactly what to connect.
				if ev.ServerID != waitServerID {
					t.Errorf("waiting event server_id = %q, want %q", ev.ServerID, waitServerID)
				}
				if len(ev.Missing) != 1 || ev.Missing[0].Provider != waitProviderName {
					t.Errorf("waiting event missing = %+v, want the provider named", ev.Missing)
				} else if ev.Missing[0].Reason != "not_connected" || ev.Missing[0].AccessLevel != "read" {
					t.Errorf("waiting event missing[0] = %+v, want reason not_connected at level read", ev.Missing[0])
				}

				// Connect the account WHILE the stream stays open — exactly
				// what the UI does when the user finishes the flow. In a
				// goroutine so this callback keeps draining the stream.
				connectOnce.Do(func() {
					go func() {
						connect := fmt.Sprintf(`{"provider":%q,"token":%q,"access_level":"read"}`,
							waitProviderName, utils.FixtureMCPToken)
						mu.Lock()
						connectedAt = time.Now()
						mu.Unlock()
						if st := c.session.DoJSON(t, http.MethodPost, "/identities/pat", strings.NewReader(connect), nil); st != http.StatusCreated {
							t.Errorf("connect the account mid-stream: got HTTP %d, want 201", st)
						}
					}()
				})
			}
			if ev.Status == "resolved" {
				mu.Lock()
				resolvedSeen = true
				mu.Unlock()
			}
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

	mu.Lock()
	defer mu.Unlock()
	if !sawWaiting {
		t.Fatalf("no toolCallWaitingAuthorization event arrived; the tool ran without waiting for a credential")
	}
	if !resolvedSeen {
		t.Errorf("the waiting event was never followed by status=resolved")
	}
	if result.ToolUseID != waitToolUse {
		t.Errorf("the tool result is for %q, want the call that waited (%q)", result.ToolUseID, waitToolUse)
	}
	if result.IsError || result.Content != utils.SecretNumber {
		t.Fatalf("tool result after authorizing = %q (is_error=%v), want %q",
			result.Content, result.IsError, utils.SecretNumber)
	}
	// And afterwards the query agrees with the runtime the other way.
	if satisfied, reason, _ := c.requirements(t); !satisfied || reason != "" {
		t.Errorf("after connecting: satisfied=%v reason=%q, want satisfied with no reason", satisfied, reason)
	}
	// A tool call is part of what happened in a conversation, so it must
	// survive the turn: reopening the thread has to show what the agent did,
	// not only what it concluded.
	c.assertToolCallsPersist(t)

	// TOOL_AUTH_RECHECK_INTERVAL is set high in dist/enact-model-inference.env
	// precisely so a fast resumption can only be explained by the Redis
	// wake-up, not by the polling safety net.
	if resumed := time.Since(connectedAt); resumed > 20*time.Second {
		t.Errorf("the call resumed %s after the credential was stored; the identity event did not wake it", resumed)
	}
}

// storedToolCall mirrors what a conversation persists for one tool call.
type storedToolCall struct {
	ServerID  string          `json:"server_id"`
	Tool      string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id"`
	Arguments json.RawMessage `json:"arguments"`
	Content   string          `json:"content"`
	IsError   bool            `json:"is_error"`
}

type storedMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []storedToolCall `json:"tool_calls"`
}

// assertToolCallsPersist sends one message through a conversation and reads
// the thread back, so the assertion is about what was STORED rather than
// what streamed.
func (c *mcpToolAuthorizationWaitCase) assertToolCallsPersist(t *utils.T) {
	var created struct {
		ID string `json:"id"`
	}
	if st := c.session.DoJSON(t, http.MethodPost, "/conversations", strings.NewReader(`{}`), &created); st != http.StatusCreated {
		t.Fatalf("create conversation: got HTTP %d, want 201", st)
	}
	defer c.session.DoJSON(t, http.MethodDelete, "/conversations/"+created.ID, nil, nil)

	body := fmt.Sprintf(`{"agent_id":%q,"content":"Call the secret_number_authed tool and reply with only the number it returns."}`, c.agentID)
	c.session.StreamSSE(t, "/conversations/"+created.ID+"/messages", strings.NewReader(body), func(event, data string) bool {
		return true // drain; the assertion is on what was persisted
	})

	var thread struct {
		Messages []storedMessage `json:"messages"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/conversations/"+created.ID, nil, &thread); st != http.StatusOK {
		t.Fatalf("reload conversation: got HTTP %d, want 200", st)
	}
	var assistant storedMessage
	for _, m := range thread.Messages {
		if m.Role == "assistant" {
			assistant = m
		}
	}
	if assistant.Role == "" {
		t.Fatalf("the reloaded conversation has no assistant message: %+v", thread.Messages)
	}
	if len(assistant.ToolCalls) == 0 {
		t.Fatalf("the stored assistant message records no tool calls; the turn's tool activity was lost")
	}
	call := assistant.ToolCalls[0]
	if call.Tool != "secret_number_authed" || call.ServerID != waitServerID || call.ToolUseID == "" {
		t.Errorf("stored tool call = %+v, want the fixture's tool with its use id", call)
	}
	if len(call.Arguments) == 0 {
		t.Errorf("the stored tool call has no arguments; a client cannot re-render what was asked")
	}
	if call.IsError || call.Content != utils.SecretNumber {
		t.Errorf("stored tool result = %q (is_error=%v), want %q", call.Content, call.IsError, utils.SecretNumber)
	}
	// The injected credential must never reach storage.
	if strings.Contains(string(call.Arguments), utils.FixtureMCPToken) {
		t.Errorf("the stored tool call carries the injected credential: %s", call.Arguments)
	}

	// A SECOND turn replays that tool call into the model's context as the
	// tool_use/tool_result pair Bedrock requires. The pairing is validated
	// upstream — an unanswered tool_use is refused — so a turn that produces
	// an answer at all is the proof the replay was well formed.
	var streamErr string
	var answered bool
	c.session.StreamSSE(t, "/conversations/"+created.ID+"/messages",
		strings.NewReader(fmt.Sprintf(`{"agent_id":%q,"content":"Without calling any tool again, what number did you already get?"}`, c.agentID)),
		func(event, data string) bool {
			switch event {
			case "error":
				streamErr = data
			case "", "message":
				answered = true
			}
			return true
		})
	if streamErr != "" {
		t.Fatalf("the second turn failed, so the replayed tool history was rejected: %s", streamErr)
	}
	if !answered {
		t.Errorf("the second turn produced no content")
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/conversations/"+created.ID, nil, &thread); st != http.StatusOK {
		t.Fatalf("reload conversation after the second turn: got HTTP %d, want 200", st)
	}
	if len(thread.Messages) != 4 {
		t.Errorf("conversation has %d messages, want 4 (two turns)", len(thread.Messages))
	}
}

func (c *mcpToolAuthorizationWaitCase) TearDown(t *utils.T) {
	if c.session != nil {
		if c.agentID != "" {
			c.session.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil)
		}
		c.session.DoJSON(t, http.MethodDelete, "/identities/"+waitProviderName, nil, nil)
	}
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id="+waitServerID, nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/providers/"+waitProviderName+"?force=true", nil, nil)
}
