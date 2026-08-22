package cases

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

const (
	// Deliberately hyphenated: a field selector cannot reach it, so this
	// exercises the cred template idiom.
	credProviderName = "e2e-mcp-cred"
	credServerID     = "e2e-mcp-authed"
	// credSharedServerID is a SECOND server declaring the same provider. The
	// user connects the account once; both servers use that one identity.
	credSharedServerID = "e2e-mcp-shared"
)

// mcpToolCredentialInjectionCase proves the platform injects a user's
// stored credential into an MCP tool call — as a header and as an argument
// — and that the platform's OWN credentials do not cross into a third
// party. It also covers the registry's validation of the configuration,
// which costs nothing (no model involved).
type mcpToolCredentialInjectionCase struct {
	utils.BaseCase
	agentID string
	// sharedAgentID runs against a different server that needs the same
	// provider.
	sharedAgentID string
}

func NewMCPToolCredentialInjection() utils.TestCase { return &mcpToolCredentialInjectionCase{} }

func (c *mcpToolCredentialInjectionCase) Name() string { return "TestMCPTools_CredentialInjection" }

// serverBody is the registration payload with a valid credential
// configuration: one tool authenticated by header, one by argument.
func (c *mcpToolCredentialInjectionCase) serverBody(t *utils.T) string {
	return fmt.Sprintf(`{
      "id": %q,
      "url": %q,
      "transport_type": "streamable-http",
      "description": "credential injection fixture",
      "tool_access_requirements": {
        "secret_number_authed": [{"provider": %q, "access_level": "read"}],
        "echo_authed":          [{"provider": %q, "access_level": "read"}],
        "header_probe":         [{"provider": %q, "access_level": "read"}]
      },
      "tool_authorizations": {
        "secret_number_authed": {"headers_authorization": [{"header_name": %q, "header_template": "Bearer {{ (cred \"%s\").Credentials }}"}]},
        "echo_authed":          {"param_authorization":   [{"param_name": "api_key", "param_template": "{{ (cred \"%s\").Credentials }}"}]},
        "header_probe":         {"headers_authorization": [{"header_name": %q, "header_template": "Bearer {{ (cred \"%s\").Credentials }}"}]}
      }
    }`,
		credServerID, t.Env.MCPFixtureURL,
		credProviderName, credProviderName, credProviderName,
		utils.FixtureAuthHeader, credProviderName,
		credProviderName,
		utils.FixtureAuthHeader, credProviderName)
}

func (c *mcpToolCredentialInjectionCase) Setup(t *utils.T) {
	c.TearDown(t)

	provider := fmt.Sprintf(`{"name":%q,"display_name":"E2E MCP credential","scheme":"bearer","access_levels":{"read":["fixture:read"]}}`, credProviderName)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		t.Env.IdentitiesURL+"/v1/providers/pat", strings.NewReader(provider), nil); st != http.StatusCreated {
		t.Fatalf("register credential provider: got HTTP %d, want 201", st)
	}
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(c.serverBody(t)), nil); st != http.StatusCreated {
		t.Fatalf("register mcp server with tool auth: got HTTP %d, want 201", st)
	}

	// A second server needing the same provider — registered BEFORE the
	// credential is stored, so nothing about it can be per-server.
	// Configured with the WILDCARD rather than by tool name: every tool this
	// server offers needs the same credential, which is the ordinary case.
	sharedServer := fmt.Sprintf(`{
      "id": %q,
      "url": %q,
      "transport_type": "streamable-http",
      "description": "second server sharing one identity, configured by wildcard",
      "tool_access_requirements": {"*": [{"provider": %q, "access_level": "read"}]},
      "tool_authorizations": {"*": {"headers_authorization": [{"header_name": %q, "header_template": "Bearer {{ (cred \"%s\").Credentials }}"}]}}
    }`, credSharedServerID, t.Env.MCPFixtureURL, credProviderName, utils.FixtureAuthHeader, credProviderName)
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(sharedServer), nil); st != http.StatusCreated {
		t.Fatalf("register the second mcp server: got HTTP %d, want 201", st)
	}

	// The credential the tools need, stored ONCE for the user the cases run
	// as — no per-server connect anywhere in this setup.
	store := fmt.Sprintf(`{"provider":%q,"credentials":{"token":%q},"access_level":"read"}`,
		credProviderName, utils.FixtureMCPToken)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		t.Env.IdentitiesURL+"/v1/identities", strings.NewReader(store), nil); st != http.StatusCreated {
		t.Fatalf("store the tool credential: got HTTP %d, want 201", st)
	}

	c.agentID = c.createAgent(t, "e2e mcp auth agent", credServerID)
	c.sharedAgentID = c.createAgent(t, "e2e mcp shared-identity agent", credSharedServerID)
}

// createAgent makes an agent bound to one MCP server and returns its id.
func (c *mcpToolCredentialInjectionCase) createAgent(t *utils.T, name, serverID string) string {
	agent := fmt.Sprintf(`{"name":%q,"model":"claude-haiku-4-5","system_prompt":"Use the tools you are given.","tools":[%q]}`, name, serverID)
	var created struct {
		ID string `json:"id"`
	}
	if st := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodPost,
		t.AgentURL("/v1/agents"), strings.NewReader(agent), &created); st != http.StatusCreated {
		t.Fatalf("create agent %q: got HTTP %d, want 201", name, st)
	}
	return created.ID
}

func (c *mcpToolCredentialInjectionCase) Run(t *utils.T) {
	c.assertConfigurationValidation(t)

	// Header path: the fixture returns the secret only when the rendered
	// header arrived.
	out := c.invoke(t, "Call the secret_number_authed tool and reply with only the number it returns.")
	if len(out.ToolCalls) == 0 {
		t.Fatalf("no tool call was made; content=%q", out.Content)
	}
	call := out.ToolCalls[0]
	if call.IsError || call.Content != utils.SecretNumber {
		t.Errorf("header-authenticated tool call = %q (is_error=%v), want %q — the credential header did not arrive",
			call.Content, call.IsError, utils.SecretNumber)
	}
	// The injected credential must never come back to the client, which is
	// free to log or store what it receives.
	if strings.Contains(string(call.Arguments), utils.FixtureMCPToken) {
		t.Errorf("the tool-call record echoes the injected credential: %s", call.Arguments)
	}

	// The SAME identity serves a different server: nothing was connected for
	// this second server, and its tool call must still be authenticated.
	// That server names no tool at all — only "*" — so this also proves the
	// wildcard reaches a real call.
	shared := c.invokeAgent(t, c.sharedAgentID, "Call the secret_number_authed tool and reply with only the number it returns.")
	if len(shared.ToolCalls) == 0 {
		t.Fatalf("no tool call was made by the second server's agent; content=%q", shared.Content)
	}
	if sc := shared.ToolCalls[0]; sc.IsError || sc.Content != utils.SecretNumber {
		t.Errorf("wildcard-configured tool call = %q (is_error=%v), want %q — one connected provider must serve every server, and \"*\" must apply to every tool",
			sc.Content, sc.IsError, utils.SecretNumber)
	}

	// Param path: the platform fills api_key, the model supplies message.
	out = c.invoke(t, `Call the echo_authed tool with message "hello" and reply with exactly what it returns.`)
	if len(out.ToolCalls) == 0 {
		t.Fatalf("no tool call was made for the param path; content=%q", out.Content)
	}
	call = out.ToolCalls[0]
	if call.IsError || !strings.Contains(call.Content, "echo: hello") {
		t.Errorf("param-authenticated tool call = %q (is_error=%v), want the echoed message — api_key was not injected",
			call.Content, call.IsError)
	}
	if strings.Contains(string(call.Arguments), utils.FixtureMCPToken) {
		t.Errorf("the tool-call record echoes the injected api_key: %s", call.Arguments)
	}

	// The platform's own credentials stop at the proxy: a third-party MCP
	// server must never see the S2S token or the impersonation header.
	out = c.invoke(t, "Call the header_probe tool and reply with exactly what it returns.")
	if len(out.ToolCalls) == 0 {
		t.Fatalf("no tool call was made for the header probe; content=%q", out.Content)
	}
	received := strings.ToLower(out.ToolCalls[0].Content)
	if !strings.Contains(received, strings.ToLower(utils.FixtureAuthHeader)) {
		t.Errorf("the fixture did not receive %s; headers were: %s", utils.FixtureAuthHeader, received)
	}
	for _, leaked := range []string{"authorization", "x-user-id", "x-enact-tool-auth"} {
		if strings.Contains(received, leaked) {
			t.Errorf("the platform leaked %q to a third-party MCP server; headers were: %s", leaked, received)
		}
	}
}

// assertConfigurationValidation covers the registry's rejection of broken
// credential configuration. No model runs, so these are free.
func (c *mcpToolCredentialInjectionCase) assertConfigurationValidation(t *utils.T) {
	base := `{"id":"e2e-mcp-invalid","url":%q,"transport_type":"streamable-http",%s}`
	cases := map[string]string{
		// A field selector cannot express a hyphenated provider name.
		"field selector on a hyphenated provider": fmt.Sprintf(
			`"tool_access_requirements":{"t":[{"provider":%q,"access_level":"read"}]},"tool_authorizations":{"t":{"headers_authorization":[{"header_name":"X-A","header_template":"{{ .%s.Credentials }}"}]}}`,
			credProviderName, credProviderName),
		// One access level per provider per tool.
		"same provider twice": fmt.Sprintf(
			`"tool_access_requirements":{"t":[{"provider":%q,"access_level":"read"},{"provider":%q,"access_level":"write"}]}`,
			credProviderName, credProviderName),
		// A template may only name providers the tool declares.
		"undeclared provider": fmt.Sprintf(
			`"tool_access_requirements":{"t":[{"provider":%q,"access_level":"read"}]},"tool_authorizations":{"t":{"headers_authorization":[{"header_name":"X-A","header_template":"{{ (cred \"somebody-else\").Credentials }}"}]}}`,
			credProviderName),
		// An inherited wildcard authorization must render against the
		// inheriting tool's OWN requirements.
		"wildcard authorization a tool cannot render": fmt.Sprintf(
			`"tool_access_requirements":{"*":[{"provider":%q,"access_level":"read"}],"other":[{"provider":"somebody-else","access_level":"read"}]},"tool_authorizations":{"*":{"headers_authorization":[{"header_name":"X-A","header_template":"{{ (cred \"%s\").Credentials }}"}]}}`,
			credProviderName, credProviderName),
		// The envelope's own namespace is reserved.
		"reserved header name": fmt.Sprintf(
			`"tool_access_requirements":{"t":[{"provider":%q,"access_level":"read"}]},"tool_authorizations":{"t":{"headers_authorization":[{"header_name":"X-Enact-Tool-Auth","header_template":"{{ (cred \"%s\").Credentials }}"}]}}`,
			credProviderName, credProviderName),
	}
	for name, tail := range cases {
		body := fmt.Sprintf(base, t.Env.MCPFixtureURL, tail)
		var out struct {
			Error string `json:"error"`
		}
		st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
			t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(body), &out)
		if st != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", name, st)
			// It registered; do not leave it behind.
			t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
				t.Env.ToolRegistryURL+"/v1/servers?id=e2e-mcp-invalid", nil, nil)
			continue
		}
		if name == "field selector on a hyphenated provider" && !strings.Contains(out.Error, "cred") {
			t.Errorf("%s: error %q does not point at the cred idiom", name, out.Error)
		}
	}
}

// inferenceResult is the non-streaming inference response the case asserts on.
type inferenceResult struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		ServerID string `json:"server_id"`
		Tool     string `json:"tool"`
		// Arguments is the model's raw argument object, kept raw so the
		// case can assert on what it does NOT contain.
		Arguments json.RawMessage `json:"arguments"`
		Content   string          `json:"content"`
		IsError   bool            `json:"is_error"`
	} `json:"tool_calls"`
	Error string `json:"error"`
}

// invoke runs one non-streaming inference turn against the agent.
func (c *mcpToolCredentialInjectionCase) invoke(t *utils.T, prompt string) inferenceResult {
	return c.invokeAgent(t, c.agentID, prompt)
}

// invokeAgent runs one non-streaming inference turn against a given agent.
func (c *mcpToolCredentialInjectionCase) invokeAgent(t *utils.T, agentID, prompt string) inferenceResult {
	body := fmt.Sprintf(`{"agent_id":%q,"messages":[{"role":"user","content":%q}],"max_tokens":200}`, agentID, prompt)
	var out inferenceResult
	st := t.DoJSON("enact-main", utils.InferenceAudience, http.MethodPost,
		t.Env.InferenceAPIURL+"/v1/inference", strings.NewReader(body), &out)
	if st != http.StatusOK {
		t.Fatalf("inference: got HTTP %d (%s), want 200", st, out.Error)
	}
	return out
}

func (c *mcpToolCredentialInjectionCase) TearDown(t *utils.T) {
	if c.agentID != "" {
		t.DeleteAgent(c.agentID)
	}
	if c.sharedAgentID != "" {
		t.DeleteAgent(c.sharedAgentID)
	}
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id="+credServerID, nil, nil)
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id="+credSharedServerID, nil, nil)
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id=e2e-mcp-invalid", nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/identities?provider="+credProviderName, nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/providers/"+credProviderName, nil, nil)
}
