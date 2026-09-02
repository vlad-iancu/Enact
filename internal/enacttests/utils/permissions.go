package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"enact/internal/identity"
	"enact/internal/logging"
)

// testRules are the permissions the suite needs that ownership cannot give
// it. Everything else a case touches, it created — and creating a resource
// grants ownership of it automatically.
//
// They are deliberately CREATE rules only, each naming a single resource
// type. A blunter grant would defeat the cases that check isolation: give the
// test user enact:agent:*:* and TestAgentManagement_DeleteIsolation would
// stop proving anything, because the user really would be allowed to reach
// another user's agent.
var testRules = []string{
	"enact:agent:create:*",
	"enact:kb:create:*",
	"enact:mcp-server:create:*",
	"enact:provider:create:*",
	"enact:workflow:create:*",
	"enact:crawl:create:*",
}

// TestRules exposes the rule set so cases can provision fixture accounts
// with exactly the same permissions as the suite's own user.
func TestRules() []string { return append([]string(nil), testRules...) }

// EnsurePermissions gives the test user the create rules the suite needs,
// before any case runs.
//
// The suite used to pass on owner bypass, which meant it was testing the
// platform as an administrator sees it and silently stopped exercising the
// rules at all. Provisioning explicitly is both more honest and more stable:
// the suite declares what it needs instead of inheriting whatever the local
// cluster happens to grant, and a run cannot be broken by someone demoting
// the test account.
//
// Idempotent — the grant endpoint deduplicates rules — so it runs at the
// start of every execution rather than once at startup, and self-heals a
// cluster whose RBAC state has drifted.
func (e *Env) EnsurePermissions(ctx context.Context, logger *logging.Logger) error {
	if e.RBACURL == "" {
		logger.Warn("no RBAC url configured; skipping permission provisioning")
		return nil
	}
	organizationID, err := e.organizationOf(ctx, e.UserID)
	if err != nil {
		return err
	}
	if organizationID == "" {
		// The one failure worth naming: it means the organization migration
		// has not been run against this cluster, and every case would fail
		// for that reason rather than anything the suite is testing.
		return fmt.Errorf("utils: the test user %q belongs to no organization; run scripts/migrate-organizations", e.UserID)
	}
	e.OrganizationID = organizationID
	if err := e.rbacPost(ctx, "/v1/grants", map[string]any{"user_id": e.UserID, "rules": testRules}); err != nil {
		return err
	}
	logger.Info("test permissions provisioned",
		"user_id", e.UserID, "organization_id", organizationID, "rules", len(testRules))
	return nil
}

// PlaceInOrganization puts a fixture account in the suite's organization, so
// it can hold identities and resources at all.
//
// Cases that create browser accounts need this: since providers became
// organization-scoped, a user who belongs to nowhere cannot connect an
// account, because there is no organization whose provider records to
// resolve against.
func (e *Env) PlaceInOrganization(ctx context.Context, userID string) error {
	if e.RBACURL == "" || e.OrganizationID == "" {
		return nil
	}
	return e.rbacPost(ctx, "/v1/memberships", map[string]any{
		"user_id":         userID,
		"organization_id": e.OrganizationID,
		"owner":           false,
	})
}

// GrantRules gives one user exactly these rules, for a case that needs a
// fixture account to hold a permission ownership cannot supply.
func (e *Env) GrantRules(ctx context.Context, userID string, rules []string) error {
	if e.RBACURL == "" {
		return nil
	}
	return e.rbacPost(ctx, "/v1/grants", map[string]any{"user_id": userID, "rules": rules})
}

// organizationOf reads a user's organization from the RBAC service.
func (e *Env) organizationOf(ctx context.Context, userID string) (string, error) {
	client, err := e.Client("enact-tests", "enact-rbac")
	if err != nil {
		return "", fmt.Errorf("utils: rbac client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.RBACURL+"/v1/effective?user_id="+url.QueryEscape(userID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(identity.Header, userID)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("utils: resolve organization: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("utils: resolve organization for %q: HTTP %d", userID, resp.StatusCode)
	}
	var out struct {
		OrganizationID string `json:"organization_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.OrganizationID, nil
}

// rbacPost issues one provisioning call and insists on 204.
func (e *Env) rbacPost(ctx context.Context, path string, body map[string]any) error {
	client, err := e.Client("enact-tests", "enact-rbac")
	if err != nil {
		return fmt.Errorf("utils: rbac client: %w", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.RBACURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.Header, e.UserID)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("utils: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("utils: %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

// RBACAudience is the authorization service, and RBACURL builds a path on
// it — the suite talks to it directly to provision fixtures and, in the
// cross-organization case, to stand up a second organization.
const RBACAudience = "enact-rbac"

func (t *T) RBACURL(path string) string { return t.Env.RBACURL + path }
