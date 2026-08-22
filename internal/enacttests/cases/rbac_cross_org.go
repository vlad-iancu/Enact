package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// crossOrgUser is a synthetic id, not an account: these cases call the
// services directly, so a user is whatever X-User-Id says it is.
const crossOrgUser = "e2e-cross-org-owner"

// rbacCrossOrgCase proves the organization boundary holds against the one
// caller who passes every permission check: another organization's OWNER.
//
// Owner bypass is unconditional by design — an owner may do anything inside
// their organization without holding a rule for it. That made every
// single-resource read reachable from any organization, because the read
// looked up by id alone and the permission check said yes. This case creates
// a second organization, makes a user its owner, and insists that every
// resource of the first is 404 to them.
//
// It also covers the symptom that exposed it: the same MCP server id must be
// registrable in two organizations, because ids are unique per organization
// rather than platform-wide.
type rbacCrossOrgCase struct {
	agent    utils.AgentDTO
	kbID     string
	serverID string
	// session owns a server registered THROUGH enact-main, so the gateway's
	// own resolution path is covered and not just the registry's.
	session       *utils.MainSession
	gatewayServer string
	// otherOrg is the organization created for this case; its owner is
	// crossOrgUser.
	otherOrg string
}

func NewRBACCrossOrg() utils.TestCase { return &rbacCrossOrgCase{} }

func (c *rbacCrossOrgCase) Name() string { return "TestRBAC_CrossOrganizationIsolation" }

// Setup creates one resource of each kind in the suite's own organization,
// then stands up a second organization with its own owner.
func (c *rbacCrossOrgCase) Setup(t *utils.T) {
	c.agent = t.CreateAgent(`{"name":"cross-org agent","model":"claude-sonnet-4-6","system_prompt":"in org A"}`)

	var kb struct {
		ID string `json:"id"`
	}
	if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodPost, t.KBURL("/v1/knowledge-bases"),
		strings.NewReader(`{"name":"cross-org kb"}`), &kb); st != http.StatusCreated {
		t.Fatalf("create kb in org A: got HTTP %d, want 201", st)
	}
	c.kbID = kb.ID

	c.serverID = "e2e-cross-org-server"
	body := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"org A"}`,
		c.serverID, t.Env.MCPFixtureURL)
	if st := t.DoJSON("enact-tests", utils.ToolRegistryAudience, http.MethodPost,
		t.ToolRegistryURL("/v1/servers/create"), strings.NewReader(body), nil); st != http.StatusCreated {
		t.Fatalf("register server in org A: got HTTP %d, want 201", st)
	}

	// A second server, registered through the browser surface by a session
	// user, so the gateway path has something of its own to resolve.
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Cross Org", mainTestEmail, mainTestPassword)
	c.gatewayServer = "e2e-cross-org-gateway"
	c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+c.gatewayServer, nil, nil)
	gateway := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"gateway path"}`,
		c.gatewayServer, t.Env.MCPFixtureURL)
	if st := c.session.DoJSON(t, http.MethodPost, "/mcp-servers", strings.NewReader(gateway), nil); st != http.StatusCreated {
		t.Fatalf("register the gateway server: got HTTP %d, want 201", st)
	}

	c.otherOrg = c.createOrganization(t)
}

// createOrganization drives the real lifecycle — request then approve —
// rather than writing a membership directly, so the case also covers the
// path that makes a requester the first owner of what they asked for.
func (c *rbacCrossOrgCase) createOrganization(t *utils.T) string {
	var request struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id"`
	}
	st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.RBACAudience, http.MethodPost,
		t.RBACURL("/v1/organizations/requests"),
		strings.NewReader(`{"name":"E2E Cross Org"}`), &request)
	switch st {
	case http.StatusCreated:
	case http.StatusConflict:
		// A previous run already placed this user in an organization; reuse it.
		var effective struct {
			OrganizationID string `json:"organization_id"`
			Owner          bool   `json:"owner"`
		}
		if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.RBACAudience, http.MethodGet,
			t.RBACURL("/v1/effective?user_id="+crossOrgUser), nil, &effective); st != http.StatusOK {
			t.Fatalf("resolve the existing organization: got HTTP %d, want 200", st)
		}
		if !effective.Owner {
			t.Fatalf("%s is in organization %q but not its owner; the case needs an owner",
				crossOrgUser, effective.OrganizationID)
		}
		return effective.OrganizationID
	default:
		t.Fatalf("request an organization: got HTTP %d, want 201 or 409", st)
	}

	var decided struct {
		OrganizationID string `json:"organization_id"`
	}
	if st := t.DoJSON("enact-tests", utils.RBACAudience, http.MethodPost,
		t.RBACURL("/v1/organizations/requests/"+request.ID+"/approve"),
		strings.NewReader(`{"reason":"e2e"}`), &decided); st != http.StatusOK {
		t.Fatalf("approve the organization request: got HTTP %d, want 200", st)
	}
	if decided.OrganizationID == "" {
		t.Fatalf("approval returned no organization id")
	}
	return decided.OrganizationID
}

func (c *rbacCrossOrgCase) Run(t *utils.T) {
	// The other organization's owner holds every permission by bypass, and
	// must still see none of this organization's resources.
	var effective struct {
		Owner bool `json:"owner"`
	}
	if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.RBACAudience, http.MethodGet,
		t.RBACURL("/v1/effective?user_id="+crossOrgUser), nil, &effective); st != http.StatusOK {
		t.Fatalf("resolve the other owner: got HTTP %d, want 200", st)
	}
	if !effective.Owner {
		t.Fatalf("%s is not an owner; the case would prove nothing", crossOrgUser)
	}

	for _, probe := range []struct {
		what     string
		audience string
		url      string
	}{
		{"agent", utils.AgentAudience, t.AgentURL("/v1/agents/" + c.agent.ID)},
		{"knowledge base", utils.KBAudience, t.KBURL("/v1/knowledge-bases/" + c.kbID)},
	} {
		st := t.DoJSONAs(crossOrgUser, "enact-tests", probe.audience, http.MethodGet, probe.url, nil, nil)
		if st != http.StatusNotFound {
			t.Errorf("another organization's owner read the %s: got HTTP %d, want 404", probe.what, st)
		}
	}

	// And the same owner must not be able to delete them either.
	if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.AgentAudience, http.MethodDelete,
		t.AgentURL("/v1/agents/"+c.agent.ID), nil, nil); st != http.StatusNotFound {
		t.Errorf("another organization's owner deleted the agent: got HTTP %d, want 404", st)
	}
	if st := t.DoJSON("enact-tests", utils.AgentAudience, http.MethodGet,
		t.AgentURL("/v1/agents/"+c.agent.ID), nil, nil); st != http.StatusOK {
		t.Errorf("the agent did not survive the cross-organization delete: got HTTP %d, want 200", st)
	}

	// The reported symptom: an id taken in one organization is free in
	// another. Registering it must succeed, not collide.
	body := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"org B"}`,
		c.serverID, t.Env.MCPFixtureURL)
	st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodPost,
		t.ToolRegistryURL("/v1/servers/create"), strings.NewReader(body), nil)
	if st != http.StatusCreated {
		t.Errorf("registering an id that exists in another organization: got HTTP %d, want 201", st)
	}
	// ...while a second registration inside the SAME organization still is a
	// conflict, so the uniqueness did not simply disappear.
	if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodPost,
		t.ToolRegistryURL("/v1/servers/create"), strings.NewReader(body), nil); st != http.StatusConflict {
		t.Errorf("duplicate id within one organization: got HTTP %d, want 409", st)
	}

	c.gatewayDeleteResolvesWithinTheOrganization(t)
}

// gatewayDeleteResolvesWithinTheOrganization covers enact-main rather than the
// registry: the gateway resolves a server by id to check the session owns it,
// and that lookup must be scoped to the caller's organization.
//
// Unscoped it matched every organization holding the same id and returned an
// arbitrary one, so the ownership check ran against a stranger's record and
// the owner was told their own server did not exist.
func (c *rbacCrossOrgCase) gatewayDeleteResolvesWithinTheOrganization(t *utils.T) {
	// The other organization registers the SAME id, so the gateway now has
	// two records to choose between.
	body := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"org B copy"}`,
		c.gatewayServer, t.Env.MCPFixtureURL)
	if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodPost,
		t.ToolRegistryURL("/v1/servers/create"), strings.NewReader(body), nil); st != http.StatusCreated {
		t.Fatalf("register the duplicate in the other organization: got HTTP %d, want 201", st)
	}

	// The owner deletes theirs through enact-main. Ambiguity here reads as
	// "not found", which is the symptom this guards against.
	if st := c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+c.gatewayServer, nil, nil); st != http.StatusNoContent {
		t.Errorf("owner deletes their own server through the gateway: got HTTP %d, want 204 — "+
			"a 404 means the lookup crossed into another organization", st)
	}

	// Theirs is gone from their listing...
	var listed struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/mcp-servers", nil, &listed); st != http.StatusOK {
		t.Fatalf("list servers after the gateway delete: got HTTP %d, want 200", st)
	}
	for _, srv := range listed.Servers {
		if srv.ID == c.gatewayServer {
			t.Errorf("the deleted server is still listed")
		}
	}
	// ...and the other organization's copy survived it.
	var others struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if st := t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodGet,
		t.ToolRegistryURL("/v1/servers?ids="+c.gatewayServer+"&owner="+crossOrgUser), nil, &others); st != http.StatusOK {
		t.Fatalf("list the other organization's servers: got HTTP %d, want 200", st)
	}
	if len(others.Servers) != 1 {
		t.Errorf("the other organization has %d copies of %s after the delete, want 1 — "+
			"a delete must not reach across the boundary", len(others.Servers), c.gatewayServer)
	}
}

func (c *rbacCrossOrgCase) TearDown(t *utils.T) {
	if c.agent.ID != "" {
		t.DeleteAgent(c.agent.ID)
	}
	if c.kbID != "" {
		t.DoJSON("enact-tests", utils.KBAudience, http.MethodDelete,
			t.KBURL("/v1/knowledge-bases/"+c.kbID), nil, nil)
	}
	if c.session != nil && c.gatewayServer != "" {
		c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+c.gatewayServer, nil, nil)
		t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodDelete,
			t.ToolRegistryURL("/v1/servers?id="+c.gatewayServer), nil, nil)
	}
	if c.serverID != "" {
		t.DoJSON("enact-tests", utils.ToolRegistryAudience, http.MethodDelete,
			t.ToolRegistryURL("/v1/servers?id="+c.serverID), nil, nil)
		t.DoJSONAs(crossOrgUser, "enact-tests", utils.ToolRegistryAudience, http.MethodDelete,
			t.ToolRegistryURL("/v1/servers?id="+c.serverID), nil, nil)
	}
}
