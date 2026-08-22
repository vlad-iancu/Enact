package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

const (
	gatedProviderName = "e2e-gate-provider"
	gatedServerID     = "e2e-gated-server"
)

// mcpGatedServerCase covers a server that authenticates its HANDSHAKE — the
// shape of the real GitHub MCP server, which answers 401 before the JSON-RPC
// body is parsed, so `initialize` and `tools/list` are refused outright.
//
// Such a server cannot be registered at all unless the platform can present
// a credential while probing it. That is what the "/initialize" and
// "/list-tools" configuration keys are for, resolved against the server's
// OWNER: at registration, and later on the background refresh where no user
// exists.
//
// It doubles as the scope-less credential case: the provider defines no
// access levels and the requirement names none, so the only question asked
// is whether the owner has connected the account at all.
type mcpGatedServerCase struct {
	utils.BaseCase
}

func NewMCPGatedServer() utils.TestCase { return &mcpGatedServerCase{} }

func (c *mcpGatedServerCase) Name() string { return "TestMCPTools_GatedServerProbe" }

// gatedURL is the fixture path that refuses an unauthenticated handshake.
func (c *mcpGatedServerCase) gatedURL(t *utils.T) string {
	return t.Env.MCPFixtureURL + utils.GatedPath
}

// registerBody builds a registration payload, optionally carrying the probe
// configuration.
func (c *mcpGatedServerCase) registerBody(t *utils.T, withProbe bool) string {
	probe := ""
	if withProbe {
		probe = fmt.Sprintf(`,
      "tool_access_requirements": {"/initialize": [{"provider": %q}]},
      "tool_authorizations": {"/initialize": {"headers_authorization": [{"header_name": %q, "header_template": "Bearer {{ (cred \"%s\").Credentials }}"}]}}`,
			gatedProviderName, utils.FixtureGateHeader, gatedProviderName)
	}
	return fmt.Sprintf(`{
      "id": %q,
      "url": %q,
      "transport_type": "streamable-http",
      "description": "a server that authenticates its handshake"%s
    }`, gatedServerID, c.gatedURL(t), probe)
}

func (c *mcpGatedServerCase) Setup(t *utils.T) {
	c.TearDown(t)
	// Deliberately WITHOUT access levels: a gate key is a key, with nothing
	// to say about scope. The requirement below names no level either, so
	// the platform asks only "has the owner connected this?".
	provider := fmt.Sprintf(`{"name":%q,"display_name":"E2E gate","scheme":"bearer"}`, gatedProviderName)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		t.Env.IdentitiesURL+"/v1/providers/pat", strings.NewReader(provider), nil); st != http.StatusCreated {
		t.Fatalf("register the gate provider: got HTTP %d, want 201", st)
	}
}

func (c *mcpGatedServerCase) Run(t *utils.T) {
	// Without the probe configuration the handshake is refused, so the
	// server cannot be registered at all. This is the state of the world for
	// GitHub's MCP server today.
	var refusal struct {
		Error string `json:"error"`
	}
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(c.registerBody(t, false)), &refusal); st != http.StatusBadGateway {
		t.Fatalf("registering a gated server with no probe credentials: got HTTP %d, want 502", st)
	}

	// Declaring the probe credentials is not enough on its own: the owner
	// has to have connected the account they name.
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(c.registerBody(t, true)), &refusal); st != http.StatusBadGateway {
		t.Errorf("registering with probe credentials the owner has not connected: got HTTP %d, want 502", st)
	} else if !strings.Contains(refusal.Error, gatedProviderName) {
		t.Errorf("the refusal %q does not name the provider the owner must connect", refusal.Error)
	}

	// Connect it as the owner — the user these service calls run as.
	connect := fmt.Sprintf(`{"provider":%q,"credentials":{"token":%q}}`,
		gatedProviderName, utils.FixtureGateToken)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		t.Env.IdentitiesURL+"/v1/identities", strings.NewReader(connect), nil); st != http.StatusCreated {
		t.Fatalf("connect the owner's gate credential: got HTTP %d, want 201", st)
	}

	// Now the probe gets through: the server registers and its tools are
	// cached, which is only possible if the handshake carried the header.
	var registered struct {
		ID        string `json:"id"`
		ToolCount int    `json:"tool_count"`
	}
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPost,
		t.Env.ToolRegistryURL+"/v1/servers/create", strings.NewReader(c.registerBody(t, true)), &registered); st != http.StatusCreated {
		t.Fatalf("registering a gated server with the owner's credentials: got HTTP %d, want 201", st)
	}
	if registered.ToolCount == 0 {
		t.Errorf("the gated server registered with %d tools; tools/list did not get through", registered.ToolCount)
	}

	// The cached catalogue is the proof that tools/list ran, not just the
	// handshake.
	var catalogue struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodGet,
		t.Env.ToolRegistryURL+"/v1/servers/tools?ids="+gatedServerID, nil, &catalogue); st != http.StatusOK {
		t.Fatalf("list cached tools: got HTTP %d, want 200", st)
	}
	if len(catalogue.Tools) != registered.ToolCount {
		t.Errorf("cached %d tools but the server reported %d", len(catalogue.Tools), registered.ToolCount)
	}

	// The probe keys are configuration, not tools: they must never appear in
	// a tool listing.
	for _, tool := range catalogue.Tools {
		if strings.HasPrefix(tool.Name, "/") {
			t.Errorf("the cached catalogue contains %q, which is a probe key rather than a tool", tool.Name)
		}
	}

	// Updating with the probe configuration removed must re-probe and fail,
	// rather than quietly keeping a server the platform can no longer reach.
	if st := t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodPut,
		t.Env.ToolRegistryURL+"/v1/servers/update?id="+gatedServerID,
		strings.NewReader(`{"tool_access_requirements":{},"tool_authorizations":{}}`), nil); st != http.StatusBadGateway {
		t.Errorf("updating away the probe credentials: got HTTP %d, want 502 — the update was not re-probed", st)
	}
}

func (c *mcpGatedServerCase) TearDown(t *utils.T) {
	t.DoJSON("enact-main", utils.ToolRegistryAudience, http.MethodDelete,
		t.Env.ToolRegistryURL+"/v1/servers?id="+gatedServerID, nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/identities?provider="+gatedProviderName, nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		t.Env.IdentitiesURL+"/v1/providers/"+gatedProviderName, nil, nil)
}
