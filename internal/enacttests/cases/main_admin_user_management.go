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
		return s, true
	case http.StatusConflict:
		login := fmt.Sprintf(`{"email":%q,"password":%q}`, t.Env.AdminEmail, adminCreatedPassword)
		if st := s.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st == http.StatusOK {
			return s, true
		}
		return nil, false
	default:
		return nil, false
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

	// The admin is designated by configuration and visible on /auth/me.
	var me struct {
		Email   string `json:"email"`
		IsAdmin bool   `json:"is_admin"`
	}
	if st := admin.DoJSON(t, http.MethodGet, "/auth/me", nil, &me); st != http.StatusOK {
		t.Fatalf("admin /auth/me: got HTTP %d, want 200", st)
	}
	if !me.IsAdmin {
		t.Fatalf("admin /auth/me: is_admin=false for %s, want true", t.Env.AdminEmail)
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

	// Deletion revokes the account's live sessions and the account itself.
	if st := admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(deleteBody), nil); st != http.StatusNoContent {
		t.Fatalf("/admin/delete-user: got HTTP %d, want 204", st)
	}
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

func (c *mainAdminUserManagementCase) TearDown(t *utils.T) {
	if c.admin == nil {
		return
	}
	// Best-effort: remove the created account if an assertion aborted Run
	// before its deletion.
	body := fmt.Sprintf(`{"email":%q}`, adminCreatedEmail)
	c.admin.DoJSON(t, http.MethodPost, "/admin/delete-user", strings.NewReader(body), nil)
}
