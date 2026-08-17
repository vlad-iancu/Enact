package utils

import (
	"net/http"
	"time"
)

// Env is what test cases run against: base URLs of the live services, the
// identity test data is created under, and (with S2S enabled) the
// impersonation fleet.
type Env struct {
	AgentAPIURL     string
	KBAPIURL        string
	InferenceAPIURL string
	ModelsAPIURL    string
	MainAPIURL      string
	ToolRegistryURL string
	IdentitiesURL   string
	RBACURL         string
	// OrganizationID is the suite's organization, resolved from the test
	// user at the start of a run. Cases use it to place fixture accounts.
	OrganizationID string

	// MCPFixtureURL is the embedded MCP fixture server's endpoint AS THE
	// REGISTRY REACHES IT (in compose, the tests container's hostname).
	MCPFixtureURL string

	// OAuthFixtureURL is the embedded fixture authorization server AS THE
	// IDENTITY SERVICE REACHES IT.
	OAuthFixtureURL string

	// UserID is sent as the X-User-Id header on every test request so test
	// data stays isolated from real users' data.
	UserID string

	// AdminEmail mirrors the platform-wide ADMIN_EMAIL setting so admin
	// cases know which account carries elevated rights. Empty when the
	// platform has no administrator configured.
	AdminEmail string

	// Fleet signs requests as arbitrary service identities; nil when S2S is
	// disabled, in which case clients are unsigned.
	Fleet *Fleet

	// Timeout bounds each HTTP call made by a test case.
	Timeout time.Duration
}

// Client returns an HTTP client acting as the given service identity. With
// S2S disabled (no fleet) it returns an unsigned client, matching the
// platform's disabled enforcement.
func (e *Env) Client(as, audience string) (*http.Client, error) {
	if e.Fleet == nil {
		return &http.Client{Timeout: e.Timeout}, nil
	}
	return e.Fleet.Client(as, audience, e.Timeout)
}
