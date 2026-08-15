package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"enact/internal/logging"
)

// The OAuth fixture is a minimal but spec-shaped authorization server
// embedded in the tests service, so the identity cases can drive a real
// authorize → callback → refresh cycle without a browser or a third party.
//
// Two deliberate properties make the refresh sweep observable in a test:
// tokens live for FixtureTokenLifetime seconds (short enough that the
// service's refresh window always covers them), and every issued access
// token carries an incrementing counter so a refreshed credential is
// visibly different from the one it replaced.

// FixtureTokenLifetime is how long (in seconds) fixture access tokens last.
const FixtureTokenLifetime = 60

// FixtureClientID / FixtureClientSecret are what a registered provider must
// present; the fixture rejects anything else so the plumbing is verified.
const (
	FixtureClientID     = "enact-test-client"
	FixtureClientSecret = "enact-test-secret"
)

type oauthFixture struct {
	mu sync.Mutex
	// codes maps an issued authorization code to the redirect_uri it was
	// issued for, so the token exchange can verify the pairing.
	codes   map[string]string
	refresh map[string]bool
	// revoked records every token the platform asked to invalidate, with the
	// token_type_hint it sent. It is what makes "did disconnecting actually
	// reach the provider?" assertable.
	revoked  map[string]string
	issued   atomic.Int64
	logger   *logging.Logger
	scopeSet string
}

// StartOAuthFixture serves the fixture authorization server on listen for
// the lifetime of the process.
func StartOAuthFixture(listen string, logger *logging.Logger) {
	f := &oauthFixture{
		codes:    map[string]string{},
		refresh:  map[string]bool{},
		revoked:  map[string]string{},
		logger:   logger,
		scopeSet: "read write",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/revoke", f.revoke)
	mux.HandleFunc("/revoked", f.revokedLookup)
	go func() {
		logger.Info("oauth fixture server listening", "addr", listen)
		if err := http.ListenAndServe(listen, mux); err != nil {
			logger.Error("oauth fixture server stopped", "err", err)
		}
	}()
}

// authorize issues a code and redirects back to the caller's redirect_uri,
// exactly as a real provider would.
func (f *oauthFixture) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	if redirectURI == "" || state == "" {
		http.Error(w, "missing redirect_uri or state", http.StatusBadRequest)
		return
	}
	code := fmt.Sprintf("fixture-code-%d", f.issued.Add(1))
	f.mu.Lock()
	f.codes[code] = redirectURI
	f.mu.Unlock()

	target, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	params := target.Query()
	params.Set("code", code)
	params.Set("state", state)
	target.RawQuery = params.Encode()
	f.logger.Info("oauth fixture: issuing code", "redirect_uri", redirectURI)
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// token handles both grant types: authorization_code and refresh_token.
func (f *oauthFixture) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, "invalid_request")
		return
	}
	// Client credentials arrive either as form params or basic auth,
	// depending on the client's auth style; accept both.
	clientID, clientSecret := r.Form.Get("client_id"), r.Form.Get("client_secret")
	if clientID == "" {
		if id, secret, ok := r.BasicAuth(); ok {
			clientID, clientSecret = id, secret
		}
	}
	if clientID != FixtureClientID || clientSecret != FixtureClientSecret {
		f.logger.Warn("oauth fixture: bad client credentials", "client_id", clientID)
		writeOAuthError(w, "invalid_client")
		return
	}

	switch grant := r.Form.Get("grant_type"); grant {
	case "authorization_code":
		code := r.Form.Get("code")
		f.mu.Lock()
		wantRedirect, ok := f.codes[code]
		delete(f.codes, code)
		f.mu.Unlock()
		if !ok {
			f.logger.Warn("oauth fixture: unknown code")
			writeOAuthError(w, "invalid_grant")
			return
		}
		if got := r.Form.Get("redirect_uri"); got != "" && got != wantRedirect {
			f.logger.Warn("oauth fixture: redirect_uri mismatch", "got", got, "want", wantRedirect)
			writeOAuthError(w, "invalid_grant")
			return
		}
		f.issueTokens(w, true)
	case "refresh_token":
		token := r.Form.Get("refresh_token")
		f.mu.Lock()
		known := f.refresh[token]
		f.mu.Unlock()
		if !known {
			f.logger.Warn("oauth fixture: unknown refresh token")
			writeOAuthError(w, "invalid_grant")
			return
		}
		// Rotate the access token but keep the refresh token, which is what
		// most providers do — and what makes "did it refresh?" observable.
		f.issueTokens(w, false)
	default:
		writeOAuthError(w, "unsupported_grant_type")
	}
}

// revoke implements RFC 7009. Like a real authorization server it requires
// client authentication, answers 200 for an unknown token (so revocation
// cannot be used to probe for one), and — the point of the fixture — makes
// a revoked refresh token stop working, so "the platform said it revoked"
// and "the grant is actually dead" are two different assertions.
func (f *oauthFixture) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, "invalid_request")
		return
	}
	clientID, clientSecret := r.Form.Get("client_id"), r.Form.Get("client_secret")
	if clientID == "" {
		if id, secret, ok := r.BasicAuth(); ok {
			clientID, clientSecret = id, secret
		}
	}
	if clientID != FixtureClientID || clientSecret != FixtureClientSecret {
		f.logger.Warn("oauth fixture: revocation with bad client credentials", "client_id", clientID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	token := r.Form.Get("token")
	if token == "" {
		writeOAuthError(w, "invalid_request")
		return
	}
	f.mu.Lock()
	f.revoked[token] = r.Form.Get("token_type_hint")
	delete(f.refresh, token)
	f.mu.Unlock()
	// The hint is logged, the token never is.
	f.logger.Info("oauth fixture: token revoked", "token_type_hint", r.Form.Get("token_type_hint"))
	w.WriteHeader(http.StatusOK)
}

// revokedLookup is fixture-only introspection: 200 when the token was
// revoked (with the hint the platform sent), 404 otherwise.
func (f *oauthFixture) revokedLookup(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	f.mu.Lock()
	hint, ok := f.revoked[token]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true, "token_type_hint": hint})
}

// issueTokens writes a token response; withRefresh mints a new refresh
// token (first consent only).
func (f *oauthFixture) issueTokens(w http.ResponseWriter, withRefresh bool) {
	n := f.issued.Add(1)
	body := map[string]any{
		"access_token": fmt.Sprintf("fixture-access-%d", n),
		"token_type":   "Bearer",
		"expires_in":   FixtureTokenLifetime,
		"scope":        f.scopeSet,
	}
	if withRefresh {
		refresh := fmt.Sprintf("fixture-refresh-%d", n)
		f.mu.Lock()
		f.refresh[refresh] = true
		f.mu.Unlock()
		body["refresh_token"] = refresh
	}
	f.logger.Info("oauth fixture: issuing tokens", "with_refresh", withRefresh)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
