package enactmain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oauthFlowCookie is the short-lived cookie carrying state + PKCE verifier
// between the authorization redirect and the callback. It is HttpOnly and
// expires with the flow; tampering only breaks the tamperer's own login
// (the state comparison is what defeats CSRF).
const oauthFlowCookie = "enact_oauth_flow"

// googleAuth wraps everything needed for the "Login with Google"
// authorization-code + PKCE flow: the OAuth2 client config and the OIDC
// verifier for Google's ID tokens.
type googleAuth struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// newGoogleAuth builds the flow against Google's published OIDC discovery
// document (fetched once at startup).
func newGoogleAuth(ctx context.Context, clientID, clientSecret, redirectURL string) (*googleAuth, error) {
	provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, fmt.Errorf("enactmain: google oidc discovery: %w", err)
	}
	return &googleAuth{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

// begin starts the flow: mints state + PKCE verifier, stores both in the
// temp cookie, and returns the Google authorization URL to redirect to.
// The flow cookie is always Lax regardless of the session cookie settings:
// it only needs to survive the top-level redirect chain to Google and back,
// which Lax permits, and Lax works without HTTPS in local dev.
func (g *googleAuth) begin(w http.ResponseWriter, secure bool) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := hex.EncodeToString(raw)
	verifier := oauth2.GenerateVerifier()

	http.SetCookie(w, &http.Cookie{
		Name:     oauthFlowCookie,
		Value:    state + "." + verifier,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return g.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// googleIdentity is what the platform reads from a validated ID token.
type googleIdentity struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// finish completes the flow on the callback: state check against the temp
// cookie, code exchange (server-to-server, with the PKCE verifier), and ID
// token validation. It returns the verified Google identity.
func (g *googleAuth) finish(r *http.Request, w http.ResponseWriter, secure bool) (googleIdentity, error) {
	var id googleIdentity

	cookie, err := r.Cookie(oauthFlowCookie)
	if err != nil {
		return id, fmt.Errorf("missing oauth flow cookie (flow not started or expired)")
	}
	// The flow cookie is single-use; clear it regardless of outcome.
	http.SetCookie(w, &http.Cookie{
		Name: oauthFlowCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})

	state, pkceVerifier, ok := strings.Cut(cookie.Value, ".")
	if !ok || state == "" || pkceVerifier == "" {
		return id, fmt.Errorf("malformed oauth flow cookie")
	}
	if q := r.URL.Query().Get("state"); q != state {
		return id, fmt.Errorf("state mismatch")
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		return id, fmt.Errorf("google returned error %q", errParam)
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return id, fmt.Errorf("callback carries no code")
	}

	token, err := g.oauth.Exchange(r.Context(), code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return id, fmt.Errorf("code exchange: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return id, fmt.Errorf("token response carries no id_token")
	}
	idToken, err := g.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		return id, fmt.Errorf("id token validation: %w", err)
	}
	if err := idToken.Claims(&id); err != nil {
		return id, fmt.Errorf("id token claims: %w", err)
	}
	if id.Sub == "" || id.Email == "" {
		return id, fmt.Errorf("id token lacks sub or email")
	}
	return id, nil
}
