package cases

import (
	"net/http"

	"enact/internal/enacttests/utils"
)

// Fixed local accounts for the enact-main session cases; RegisterOrLogin
// makes reruns idempotent.
const (
	mainTestEmail    = "e2e-main@example.com"
	mainOtherEmail   = "e2e-other@example.com"
	mainTestPassword = "integration-tests-pw"
)

// mainAuthSessionLifecycleCase verifies enact-main's session machinery from
// a browser-like client: register/login, session-bound /auth/me, guarded
// routes without a session, and logout invalidation.
type mainAuthSessionLifecycleCase struct {
	utils.BaseCase
}

func NewMainAuthSessionLifecycle() utils.TestCase { return &mainAuthSessionLifecycleCase{} }

func (c *mainAuthSessionLifecycleCase) Name() string { return "TestMainAuth_SessionLifecycle" }

func (c *mainAuthSessionLifecycleCase) Run(t *utils.T) {
	// Guarded routes reject session-less browsers.
	anon := t.NewMainSession()
	if st := anon.DoJSON(t, http.MethodGet, "/agents", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("no session -> /agents: got HTTP %d, want 401", st)
	}
	if st := anon.DoJSON(t, http.MethodGet, "/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("no session -> /auth/me: got HTTP %d, want 401", st)
	}

	s := t.NewMainSession()
	s.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	var me struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/auth/me", nil, &me); st != http.StatusOK {
		t.Fatalf("/auth/me with session: got HTTP %d, want 200", st)
	}
	if me.Email != mainTestEmail || !me.EmailVerified {
		t.Errorf("/auth/me: email %q verified %v, want %q true", me.Email, me.EmailVerified, mainTestEmail)
	}

	if st := s.DoJSON(t, http.MethodPost, "/auth/logout", nil, nil); st != http.StatusNoContent {
		t.Errorf("logout: got HTTP %d, want 204", st)
	}
	if st := s.DoJSON(t, http.MethodGet, "/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("/auth/me after logout: got HTTP %d, want 401", st)
	}
}
