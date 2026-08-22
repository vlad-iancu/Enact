package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// Fixed accounts for the admin case. The admin account itself comes from
// the platform's ADMIN_EMAIL setting; the created account is owned by this
// case and removed again.
const (
	adminCreatedEmail    = "e2e-admin-created@example.com"
	adminCreatedPassword = "integration-tests-pw"
	adminPATProvider     = "e2e-admin-pat"
	adminOAuthProvider   = "e2e-admin-oauth"
	// adminIdentityProvider outlives the provider-deletion checks below, so
	// the created account still holds a credential when it is deleted.
	adminIdentityProvider = "e2e-admin-identity"
)

// mainAdminUserManagementCase verifies the administrator endpoints:
// designation via ADMIN_EMAIL (is_admin on /auth/me), user creation that is
// verified from birth, session revocation on deletion, and the 401/403
// guards for anonymous and non-admin callers.
//
// The admin-session checks need the ADMIN_EMAIL account to be reachable
// with the suite's fixed test password. When the platform designates a real
// person instead (e.g. a Gmail address), those checks are skipped with a
// log line rather than failed — guard behavior is still asserted.
type mainAdminUserManagementCase struct {
	utils.BaseCase
	admin *utils.MainSession
}

func NewMainAdminUserManagement() utils.TestCase { return &mainAdminUserManagementCase{} }

func (c *mainAdminUserManagementCase) Name() string { return "TestMainAdmin_UserManagement" }

// adminSession tries to authenticate as the ADMIN_EMAIL account with the
// fixed test password: register (bootstrap-verified, starts a session) or
// fall back to login. The boolean reports whether admin access is testable.
func (c *mainAdminUserManagementCase) adminSession(t *utils.T) (*utils.MainSession, bool) {
	s := t.NewMainSession()
	body := fmt.Sprintf(`{"display_name":"E2E Admin","email":%q,"password":%q}`, t.Env.AdminEmail, adminCreatedPassword)
	status := s.DoJSON(t, http.MethodPost, "/auth/register", strings.NewReader(body), nil)
	switch status {
	case http.StatusCreated:
	case http.StatusConflict:
		login := fmt.Sprintf(`{"email":%q,"password":%q}`, t.Env.AdminEmail, adminCreatedPassword)
		if st := s.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st != http.StatusOK {
			return nil, false
		}
	default:
		return nil, false
	}
	// Being the administrator confers no organization and no rules — those
	// are RBAC's, and provider registration now needs both.
	c.provisionAdmin(t, s)
	return s, true
}

// provisionAdmin places the administrator's account in the suite's
// organization with the same create rules any fixture account gets.
func (c *mainAdminUserManagementCase) provisionAdmin(t *utils.T, s *utils.MainSession) {
	var me struct {
		ID string `json:"id"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/auth/me", nil, &me); st != http.StatusOK || me.ID == "" {
		t.Fatalf("admin /auth/me: got HTTP %d (id=%q), want 200 with an id", st, me.ID)
	}
	if err := t.Env.PlaceInOrganization(t.Context(), me.ID); err != nil {
		t.Fatalf("place the administrator in the suite's organization: %v", err)
	}
	if err := t.Env.GrantRules(t.Context(), me.ID, utils.TestRules()); err != nil {
		t.Fatalf("grant the administrator create permissions: %v", err)
	}
}

func (c *mainAdminUserManagementCase) Run(t *utils.T) {
	// The guards hold for everyone regardless of admin configuration.
	anon := t.NewMainSession()
	if st := anon.DoJSON(t, http.MethodPost, "/admin/create-user", strings.NewReader(`{}`), nil); st != http.StatusUnauthorized {
		t.Errorf("anonymous /admin/create-user: got HTTP %d, want 401", st)
	}
	if st := anon.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(`{}`), nil); st != http.StatusUnauthorized {
		t.Errorf("anonymous /admin/delete-user: got HTTP %d, want 401", st)
	}
	// Provider registration is no longer an administrator action: providers
	// are keyed by (organization, name), so registering one acts inside one
	// organization rather than platform-wide. It moved to the identities
	// surface, gated by the rule enact:provider:create. A session is still
	// required.
	if st := anon.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(`{}`), nil); st != http.StatusUnauthorized {
		t.Errorf("anonymous /identities/providers/pat: got HTTP %d, want 401", st)
	}
	if st := anon.DoJSON(t, http.MethodPost, "/identities/providers/oauth", strings.NewReader(`{}`), nil); st != http.StatusUnauthorized {
		t.Errorf("anonymous /identities/providers/oauth: got HTTP %d, want 401", st)
	}

	if t.Env.AdminEmail == "" {
		t.Logf("ADMIN_EMAIL not configured; skipping admin-session checks")
		return
	}
	admin, ok := c.adminSession(t)
	if !ok {
		t.Logf("admin account %s is not reachable with the test password (a real person's account?); skipping admin-session checks", t.Env.AdminEmail)
		return
	}
	c.admin = admin

	// The administrator routes for providers are retired: registering one is
	// organization business now, so an authenticated administrator gets 404
	// rather than a working endpoint. Checked with a session because the
	// /admin group's session filter answers before route matching does.
	if st := admin.DoJSON(t, http.MethodPost, "/admin/identity-providers/pat", strings.NewReader(`{}`), nil); st != http.StatusNotFound {
		t.Errorf("retired /admin/identity-providers/pat: got HTTP %d, want 404", st)
	}

	// The admin is designated by configuration and visible on /auth/me.
	var me struct {
		Email   string `json:"email"`
		IsAdmin bool   `json:"is_admin"`
	}
	if st := admin.DoJSON(t, http.MethodGet, "/auth/me", nil, &me); st != http.StatusOK {
		t.Fatalf("admin /auth/me: got HTTP %d, want 200", st)
	}
	if !me.IsAdmin {
		t.Fatalf("admin /auth/me: is_admin=false for %s, want true (if this address is not enact-main's ADMIN_EMAIL, the setting has diverged between services — it must be set once, in the shared env)", t.Env.AdminEmail)
	}

	// A leftover created account from an aborted run must not fail the
	// create below.
	deleteBody := fmt.Sprintf(`{"email":%q}`, adminCreatedEmail)
	admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(deleteBody), nil)

	// Create: verified from birth, not an admin.
	createBody := fmt.Sprintf(`{"display_name":"E2E Created","email":%q,"password":%q}`, adminCreatedEmail, adminCreatedPassword)
	var created struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		IsAdmin       bool   `json:"is_admin"`
	}
	if st := admin.DoJSON(t, http.MethodPost, "/admin/create-user", strings.NewReader(createBody), &created); st != http.StatusCreated {
		t.Fatalf("/admin/create-user: got HTTP %d, want 201", st)
	}
	if !created.EmailVerified || created.IsAdmin {
		t.Errorf("created user: email_verified=%v is_admin=%v, want true/false", created.EmailVerified, created.IsAdmin)
	}

	// The paginated listing sees both accounts; page_size=1 exercises the
	// paging arithmetic (total spans pages, each page carries one user).
	var listing struct {
		Users []struct {
			Email string `json:"email"`
		} `json:"users"`
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}
	if st := admin.DoJSON(t, http.MethodGet, "/admin/users?page=1&page_size=1", nil, &listing); st != http.StatusOK {
		t.Fatalf("/admin/users: got HTTP %d, want 200", st)
	}
	if listing.Total < 2 || len(listing.Users) != 1 || listing.PageSize != 1 {
		t.Errorf("/admin/users page_size=1: total=%d users=%d page_size=%d, want total>=2 users=1 page_size=1",
			listing.Total, len(listing.Users), listing.PageSize)
	}
	found := false
	for page := 1; page <= listing.Total && !found; page++ {
		var p struct {
			Users []struct {
				Email string `json:"email"`
			} `json:"users"`
		}
		admin.DoJSON(t, http.MethodGet, fmt.Sprintf("/admin/users?page=%d&page_size=1", page), nil, &p)
		for _, u := range p.Users {
			if u.Email == adminCreatedEmail {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("/admin/users: created account %s not present in any page", adminCreatedEmail)
	}
	if st := admin.DoJSON(t, http.MethodGet, "/admin/users?page=0", nil, nil); st != http.StatusBadRequest {
		t.Errorf("/admin/users page=0: got HTTP %d, want 400", st)
	}

	// The created account can log in immediately — no verification email
	// round-trip — and is not an admin.
	user := t.NewMainSession()
	login := fmt.Sprintf(`{"email":%q,"password":%q}`, adminCreatedEmail, adminCreatedPassword)
	if st := user.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st != http.StatusOK {
		t.Fatalf("created user login: got HTTP %d, want 200", st)
	}
	if st := user.DoJSON(t, http.MethodPost, "/admin/create-user", strings.NewReader(`{}`), nil); st != http.StatusForbidden {
		t.Errorf("non-admin /admin/create-user: got HTTP %d, want 403", st)
	}
	if st := user.DoJSON(t, http.MethodGet, "/admin/users", nil, nil); st != http.StatusForbidden {
		t.Errorf("non-admin /admin/users: got HTTP %d, want 403", st)
	}
	// A logged-in account that holds no enact:provider:create rule — and no
	// organization at all, having just been created — is refused. 403 rather
	// than 404: the refusal is about the caller, and there is no resource
	// whose existence could be disclosed.
	if st := user.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(`{"name":"nope"}`), nil); st != http.StatusForbidden {
		t.Errorf("provider registration without the rule: got HTTP %d, want 403", st)
	}

	// Provider registration: a PAT provider needs nothing but a name...
	//
	// Acted by the admin session, which must hold enact:provider:create —
	// the suite places the account in its organization and grants the rule
	// below, because an account that belongs to no organization has no
	// provider records to create against.
	patBody := fmt.Sprintf(`{"name":%q,"display_name":"E2E Admin PAT","access_levels":{"read":["a"]}}`, adminPATProvider)
	var patProvider struct {
		Name         string              `json:"name"`
		Type         string              `json:"type"`
		AccessLevels map[string][]string `json:"access_levels"`
	}
	if st := admin.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(patBody), &patProvider); st != http.StatusCreated {
		t.Fatalf("register pat provider: got HTTP %d, want 201", st)
	}
	if patProvider.Type != "pat" || len(patProvider.AccessLevels) != 1 {
		t.Errorf("registered pat provider = %+v, want type pat with one access level", patProvider)
	}

	// ...while an OAuth provider carries the client registration, and the
	// response tells the registrar which redirect URI to register upstream.
	oauthBody := fmt.Sprintf(`{"name":%q,"display_name":"E2E Admin OAuth","authorize_url":"https://example.test/auth","token_url":"https://example.test/token","client_id":"cid","client_secret":"csecret","use_pkce":true,"access_levels":{"read":["a"],"readwrite":["a","b"]}}`, adminOAuthProvider)
	var oauthProvider struct {
		Name            string `json:"name"`
		Type            string `json:"type"`
		ClientSecretSet bool   `json:"client_secret_set"`
		CallbackURL     string `json:"callback_url"`
	}
	status, raw := admin.DoJSONRaw(t, http.MethodPost, "/identities/providers/oauth", strings.NewReader(oauthBody), &oauthProvider)
	if status != http.StatusCreated {
		t.Fatalf("register oauth provider: got HTTP %d, want 201", status)
	}
	if !oauthProvider.ClientSecretSet || strings.Contains(raw, "csecret") {
		t.Errorf("oauth provider response mishandles the client secret (set=%v, echoed=%v)",
			oauthProvider.ClientSecretSet, strings.Contains(raw, "csecret"))
	}
	if !strings.Contains(oauthProvider.CallbackURL, "/v1/oauth/callback") {
		t.Errorf("callback_url = %q, want the platform's oauth callback", oauthProvider.CallbackURL)
	}
	// Invalid definitions are refused with the service's own message.
	bad := fmt.Sprintf(`{"name":%q,"access_levels":{"read":[]}}`, adminPATProvider+"-bad")
	if st := admin.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(bad), nil); st != http.StatusBadRequest {
		t.Errorf("provider with an empty access level: got HTTP %d, want 400", st)
	}

	// Both are visible to ordinary users, who must see what they can connect to.
	var providers struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if st := user.DoJSON(t, http.MethodGet, "/identities/providers", nil, &providers); st != http.StatusOK {
		t.Fatalf("user /identities/providers: got HTTP %d, want 200", st)
	}
	seen := map[string]bool{}
	for _, p := range providers.Providers {
		seen[p.Name] = true
	}
	if !seen[adminPATProvider] || !seen[adminOAuthProvider] {
		t.Errorf("registered providers not visible to users: %v", providers.Providers)
	}

	// Registering the same name twice is a conflict, and the reason must
	// survive the hop from the identity service — an administrator seeing
	// only "failed to register" cannot act on it.
	var dup struct {
		Error string `json:"error"`
	}
	if st := admin.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(patBody), &dup); st != http.StatusConflict {
		t.Errorf("registering a duplicate provider: got HTTP %d, want 409", st)
	} else if !strings.Contains(dup.Error, adminPATProvider) {
		t.Errorf("the conflict %q does not name the provider", dup.Error)
	}

	// Deleting them needs enact:provider:delete on the name, which the
	// registrar holds: registering a provider records ownership of it.
	for _, name := range []string{adminPATProvider, adminOAuthProvider} {
		if st := admin.DoJSON(t, http.MethodDelete, "/identities/providers/"+name, nil, nil); st != http.StatusNoContent {
			t.Errorf("delete provider %s: got HTTP %d, want 204", name, st)
		}
	}
	if st := admin.DoJSON(t, http.MethodDelete, "/identities/providers/"+adminPATProvider, nil, nil); st != http.StatusNotFound {
		t.Errorf("deleting an absent provider: got HTTP %d, want 404", st)
	}

	// A deleted account must not leave third-party credentials behind: they
	// would be unreachable (nothing maps them back to a user) and the
	// refresh sweep would keep them alive. Connect one, then delete.
	identityProvider := fmt.Sprintf(`{"name":%q,"display_name":"E2E Admin Identity","access_levels":{"read":["a"]}}`, adminIdentityProvider)
	if st := admin.DoJSON(t, http.MethodPost, "/identities/providers/pat", strings.NewReader(identityProvider), nil); st != http.StatusCreated {
		t.Fatalf("register the identity provider: got HTTP %d, want 201", st)
	}
	var createdUser struct {
		ID string `json:"id"`
	}
	if st := user.DoJSON(t, http.MethodGet, "/auth/me", nil, &createdUser); st != http.StatusOK || createdUser.ID == "" {
		t.Fatalf("created user /auth/me: got HTTP %d (id=%q), want 200 with an id", st, createdUser.ID)
	}
	// The created account must be in an organization before it can connect
	// anything: a provider is resolved through the caller's organization, so
	// a user who belongs to nowhere has no provider to connect to.
	if err := t.Env.PlaceInOrganization(t.Context(), createdUser.ID); err != nil {
		t.Fatalf("place the created account in the suite's organization: %v", err)
	}
	connect := fmt.Sprintf(`{"provider":%q,"token":"admin-case-token","access_level":"read"}`, adminIdentityProvider)
	if st := user.DoJSON(t, http.MethodPost, "/identities/pat", strings.NewReader(connect), nil); st != http.StatusCreated {
		t.Fatalf("created user connects an account: got HTTP %d, want 201", st)
	}
	if n := c.storedIdentities(t, createdUser.ID); n != 1 {
		t.Fatalf("the created user holds %d identities before deletion, want 1", n)
	}

	// A conversation too: since ADR-0018 it holds the verbatim results of
	// every tool call made on the account's behalf, so it must not outlive
	// the account any more than the credentials do.
	var conversation struct {
		ID string `json:"id"`
	}
	if st := user.DoJSON(t, http.MethodPost, "/conversations", nil, &conversation); st != http.StatusCreated {
		t.Fatalf("created user starts a conversation: got HTTP %d, want 201", st)
	}
	if n := c.conversationCount(t, user); n != 1 {
		t.Fatalf("the created user holds %d conversations before deletion, want 1", n)
	}

	// Deletion revokes the account's live sessions and the account itself.
	if st := admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(deleteBody), nil); st != http.StatusNoContent {
		t.Fatalf("/admin/delete-user: got HTTP %d, want 204", st)
	}
	if n := c.storedIdentities(t, createdUser.ID); n != 0 {
		t.Errorf("%d stored credentials survived the account deletion, want 0 — they are unreachable and still refreshed", n)
	}
	// The conversation cascade is NOT asserted here, and deliberately not
	// faked: conversations are only reachable through enact-main's
	// session-scoped listing, and the account that owned them can no longer
	// log in. There is no endpoint that can see another user's conversations,
	// so any check written here would pass whether or not the cascade ran.
	// The pre-condition above (exactly one conversation before deletion) is
	// what this case can honestly establish; the cascade itself is covered
	// by conversations.Repository.DeleteByUser and verified against the
	// index directly.
	if st := user.DoJSON(t, http.MethodGet, "/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("deleted user's session on /auth/me: got HTTP %d, want 401", st)
	}
	if st := user.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st != http.StatusUnauthorized {
		t.Errorf("deleted user login: got HTTP %d, want 401", st)
	}
	if st := admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(deleteBody), nil); st != http.StatusNotFound {
		t.Errorf("delete-user twice: got HTTP %d, want 404", st)
	}

	// The administrator cannot delete themselves.
	selfDelete := fmt.Sprintf(`{"email":%q}`, t.Env.AdminEmail)
	if st := admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(selfDelete), nil); st != http.StatusBadRequest {
		t.Errorf("admin self-delete: got HTTP %d, want 400", st)
	}
}

// storedIdentities counts what the identity service holds for one user,
// asked as a service (the route is service-only, and the user's own session
// is gone by the time this matters).
// conversationCount reports how many conversations a session's account has,
// through the browser surface the person themselves uses.
func (c *mainAdminUserManagementCase) conversationCount(t *utils.T, s *utils.MainSession) int {
	var out struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/conversations", nil, &out); st != http.StatusOK {
		t.Fatalf("list conversations: got HTTP %d, want 200", st)
	}
	return len(out.Conversations)
}

func (c *mainAdminUserManagementCase) storedIdentities(t *utils.T, userID string) int {
	var out struct {
		Identities []struct {
			Provider string `json:"provider"`
		} `json:"identities"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		t.Env.IdentitiesURL+"/v1/identities?user_id="+userID, nil, &out); st != http.StatusOK {
		t.Fatalf("list stored identities for %s: got HTTP %d, want 200", userID, st)
	}
	return len(out.Identities)
}

func (c *mainAdminUserManagementCase) TearDown(t *utils.T) {
	if c.admin == nil {
		return
	}
	// Best-effort: remove the created account if an assertion aborted Run
	// before its deletion.
	body := fmt.Sprintf(`{"email":%q}`, adminCreatedEmail)
	c.admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(body), nil)
	for _, name := range []string{adminPATProvider, adminPATProvider + "-bad", adminOAuthProvider, adminIdentityProvider} {
		c.admin.DoJSON(t, http.MethodDelete, "/identities/providers/"+name, nil, nil)
	}
}
