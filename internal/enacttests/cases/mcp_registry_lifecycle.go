package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
	"enact/internal/mcp"
)

const registryLifecycleServerID = "e2e-mcp-lifecycle"

// mcpRegistryLifecycleCase exercises the MCP registry through enact-main
// (the user-facing management surface) against the embedded fixture server:
// registration health-checks the endpoint and caches its tools, listing and
// partial update behave, the MCP proxy speaks real MCP, and deletion
// removes the cached tools with the server.
type mcpRegistryLifecycleCase struct {
	utils.BaseCase
	session *utils.MainSession
}

func NewMCPRegistryLifecycle() utils.TestCase { return &mcpRegistryLifecycleCase{} }

func (c *mcpRegistryLifecycleCase) Name() string { return "TestMCPRegistry_ServerLifecycle" }

func (c *mcpRegistryLifecycleCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E MCP", mainTestEmail, mainTestPassword)
	// A leftover server from an aborted run must not fail registration.
	c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+registryLifecycleServerID, nil, nil)
}

func (c *mcpRegistryLifecycleCase) Run(t *utils.T) {
	// Registering an unreachable endpoint fails the health check.
	badBody := `{"id":"e2e-mcp-unreachable","url":"http://localhost:1/nope","transport_type":"streamable-http","description":"dead"}`
	if st := c.session.DoJSON(t, http.MethodPost, "/mcp-servers", strings.NewReader(badBody), nil); st != http.StatusBadRequest {
		t.Errorf("register unreachable server: got HTTP %d, want 400", st)
	}

	// Registering the live fixture succeeds and caches its tools.
	createBody := fmt.Sprintf(`{"id":%q,"url":%q,"transport_type":"streamable-http","description":"e2e fixture"}`,
		registryLifecycleServerID, t.Env.MCPFixtureURL)
	var created struct {
		ID        string `json:"id"`
		ToolCount int    `json:"tool_count"`
		Owner     string `json:"owner"`
	}
	if st := c.session.DoJSON(t, http.MethodPost, "/mcp-servers", strings.NewReader(createBody), &created); st != http.StatusCreated {
		t.Fatalf("register mcp server: got HTTP %d, want 201", st)
	}
	if created.ToolCount < 2 {
		t.Errorf("registered server: tool_count=%d, want >=2 (secret_number, echo)", created.ToolCount)
	}
	if created.Owner == "" {
		t.Errorf("registered server has no owner")
	}

	// The listing shows it; the tool cache has the fixture's tools with
	// their schemas.
	var listing struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/mcp-servers", nil, &listing); st != http.StatusOK {
		t.Fatalf("list mcp servers: got HTTP %d, want 200", st)
	}
	found := false
	for _, s := range listing.Servers {
		if s.ID == registryLifecycleServerID {
			found = true
		}
	}
	if !found {
		t.Errorf("registered server missing from /mcp-servers listing")
	}
	var toolsOut struct {
		Tools []struct {
			ServerID    string `json:"server_id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/mcp-servers/tools", nil, &toolsOut); st != http.StatusOK {
		t.Fatalf("list mcp tools: got HTTP %d, want 200", st)
	}
	hasSecret := false
	for _, tool := range toolsOut.Tools {
		if tool.ServerID == registryLifecycleServerID && tool.Name == "secret_number" {
			hasSecret = true
		}
	}
	if !hasSecret {
		t.Errorf("tool cache is missing secret_number for %s", registryLifecycleServerID)
	}

	// Partial update: description only, endpoint untouched.
	updateBody := `{"description":"updated by e2e"}`
	var updated struct {
		Description string `json:"description"`
		URL         string `json:"url"`
	}
	if st := c.session.DoJSON(t, http.MethodPut, "/mcp-servers/"+registryLifecycleServerID, strings.NewReader(updateBody), &updated); st != http.StatusOK {
		t.Fatalf("update mcp server: got HTTP %d, want 200", st)
	}
	if updated.Description != "updated by e2e" || updated.URL != t.Env.MCPFixtureURL {
		t.Errorf("update: description=%q url=%q, want updated description and unchanged url", updated.Description, updated.URL)
	}

	// The proxy is a spec-compliant MCP endpoint: a real MCP client
	// connected through it sees the fixture's tools.
	proxyURL := t.Env.ToolRegistryURL + "/v1/servers/" + registryLifecycleServerID + "/mcp"
	session, err := mcp.Connect(t.Context(), proxyURL, mcp.TransportStreamableHTTP, nil)
	if err != nil {
		t.Fatalf("mcp connect through proxy: %v", err)
	}
	defer func() { _ = session.Close() }()
	proxied, err := session.ListTools(t.Context())
	if err != nil {
		t.Fatalf("list tools through proxy: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range proxied {
		names[tool.Name] = true
	}
	if !names["secret_number"] || !names["echo"] {
		t.Errorf("proxy tool list %v, want secret_number and echo", names)
	}

	// Calling a tool through the proxy round-trips arguments.
	res, err := session.CallTool(t.Context(), "echo", []byte(`{"message":"proxied"}`))
	if err != nil {
		t.Fatalf("call tool through proxy: %v", err)
	}
	if res.Content != "echo: proxied" || res.IsError {
		t.Errorf("proxied echo: content=%q is_error=%v, want \"echo: proxied\" false", res.Content, res.IsError)
	}

	// Deletion removes the server and its cached tools.
	if st := c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+registryLifecycleServerID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete mcp server: got HTTP %d, want 204", st)
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/mcp-servers/tools", nil, &toolsOut); st != http.StatusOK {
		t.Fatalf("list tools after delete: got HTTP %d, want 200", st)
	}
	for _, tool := range toolsOut.Tools {
		if tool.ServerID == registryLifecycleServerID {
			t.Errorf("cached tool %q survived server deletion", tool.Name)
		}
	}
}

func (c *mcpRegistryLifecycleCase) TearDown(t *utils.T) {
	if c.session == nil {
		return
	}
	c.session.DoJSON(t, http.MethodDelete, "/mcp-servers/"+registryLifecycleServerID, nil, nil)
}
