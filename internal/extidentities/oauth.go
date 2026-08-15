package extidentities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// oauthEnvelope is the JSON persisted (sealed) as Identity.Credentials for
// OAuth identities. It keeps everything the callback returned — the refresh
// token the sweep needs, the expiry (also lifted to a plain field), and the
// raw extras (id_token and provider-specific fields) so a future consumer
// is not limited by what this service parses today.
type oauthEnvelope struct {
	Version      int            `json:"v"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	Expiry       *time.Time     `json:"expiry,omitempty"`
	Scope        []string       `json:"scope,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
	ObtainedAt   time.Time      `json:"obtained_at"`
}

// oauthProvider implements Provider for authorization-code identities.
type oauthProvider struct {
	record ProviderRecord
	cfg    *oauth2.Config
	hc     *http.Client
}

// OAuthFlow is implemented by OAuth providers: the consent URL and the code
// exchange that the service's authorize/callback handlers drive. It is not
// part of Provider because PAT providers have no flow.
type OAuthFlow interface {
	// AuthCodeURL builds the provider's consent URL. verifier may be empty
	// when the provider record disables PKCE.
	AuthCodeURL(state, redirectURI string, scopes []string, verifier string) string
	// Exchange trades an authorization code for the callback tuple, in the
	// same map shape StoreIdentity accepts.
	Exchange(ctx context.Context, code, redirectURI, verifier string) (map[string]any, error)
	// Scopes returns the provider's default scopes.
	Scopes() []string
	// UsePKCE reports whether the provider record enables PKCE.
	UsePKCE() bool
}

func newOAuthProvider(rec ProviderRecord, vault *Vault, hc *http.Client) (Provider, error) {
	if rec.OAuth == nil {
		return nil, fmt.Errorf("extidentities: provider %q has no oauth configuration", rec.Name)
	}
	secret, err := vault.Open(rec.OAuth.ClientSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("extidentities: provider %q client secret: %w", rec.Name, err)
	}
	authStyle := oauth2.AuthStyleAutoDetect
	switch strings.ToLower(rec.OAuth.AuthStyle) {
	case "header":
		authStyle = oauth2.AuthStyleInHeader
	case "params":
		authStyle = oauth2.AuthStyleInParams
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &oauthProvider{
		record: rec,
		hc:     hc,
		cfg: &oauth2.Config{
			ClientID:     rec.OAuth.ClientID,
			ClientSecret: secret,
			Scopes:       rec.OAuth.Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   rec.OAuth.AuthorizeURL,
				TokenURL:  rec.OAuth.TokenURL,
				AuthStyle: authStyle,
			},
		},
	}, nil
}

func (p *oauthProvider) Name() string       { return p.record.Name }
func (p *oauthProvider) Type() ProviderType { return ProviderTypeOAuth }
func (p *oauthProvider) Scopes() []string   { return p.record.OAuth.Scopes }
func (p *oauthProvider) UsePKCE() bool      { return p.record.OAuth.UsePKCE }

// ctxWithClient hands the oauth2 package the transport to use, so token
// exchanges and refreshes join the caller's trace.
func (p *oauthProvider) ctxWithClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, p.hc)
}

// AuthCodeURL builds the consent URL, including the access_type/prompt
// parameters that decide whether the provider issues a refresh token.
func (p *oauthProvider) AuthCodeURL(state, redirectURI string, scopes []string, verifier string) string {
	cfg := *p.cfg
	cfg.RedirectURL = redirectURI
	if len(scopes) > 0 {
		cfg.Scopes = scopes
	}
	opts := []oauth2.AuthCodeOption{}
	if p.record.OAuth.AccessType != "" {
		opts = append(opts, oauth2.SetAuthURLParam("access_type", p.record.OAuth.AccessType))
	}
	if p.record.OAuth.Prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", p.record.OAuth.Prompt))
	}
	for k, v := range p.record.OAuth.ExtraAuthParams {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	if verifier != "" {
		opts = append(opts, oauth2.S256ChallengeOption(verifier))
	}
	return cfg.AuthCodeURL(state, opts...)
}

// Exchange trades the authorization code for tokens and returns the tuple
// as a map, which is exactly what StoreIdentity consumes.
func (p *oauthProvider) Exchange(ctx context.Context, code, redirectURI, verifier string) (map[string]any, error) {
	cfg := *p.cfg
	cfg.RedirectURL = redirectURI
	opts := []oauth2.AuthCodeOption{}
	if verifier != "" {
		opts = append(opts, oauth2.VerifierOption(verifier))
	}
	token, err := cfg.Exchange(p.ctxWithClient(ctx), code, opts...)
	if err != nil {
		return nil, exchangeError("code exchange", err)
	}
	return tokenToPayload(token), nil
}

// StoreIdentity normalizes a callback tuple into the stored envelope.
func (p *oauthProvider) StoreIdentity(_ context.Context, payload any, previous string) (StoredCredential, error) {
	fields, err := toStringKeyedMap(payload)
	if err != nil {
		return StoredCredential{}, err
	}

	env := oauthEnvelope{Version: 1, ObtainedAt: time.Now().UTC(), Extra: map[string]any{}}
	for k, v := range fields {
		switch k {
		case "access_token":
			env.AccessToken, _ = v.(string)
		case "refresh_token":
			env.RefreshToken, _ = v.(string)
		case "token_type":
			env.TokenType, _ = v.(string)
		case "expiry":
			env.Expiry = parseExpiry(v)
		case "expires_in":
			if secs, ok := toFloat(v); ok && secs > 0 {
				t := time.Now().UTC().Add(time.Duration(secs) * time.Second)
				env.Expiry = &t
			}
		case "scope", "scopes":
			env.Scope = toStringSlice(v)
		default:
			env.Extra[k] = v
		}
	}
	if env.AccessToken == "" {
		return StoredCredential{}, fmt.Errorf("extidentities: oauth payload has no access_token")
	}
	// A provider typically issues a refresh token only on first consent.
	// Re-authorizing without one must not destroy the working one.
	if env.RefreshToken == "" && previous != "" {
		if prev, err := decodeOAuthEnvelope(previous); err == nil {
			env.RefreshToken = prev.RefreshToken
		}
	}
	// Whether the provider itself told us what it granted decides how much
	// the recorded access can be trusted — and whether a caller's requested
	// scopes may override it (they may not).
	grantedByProvider := len(env.Scope) > 0
	if !grantedByProvider {
		// The provider was silent, so the best available guess is what this
		// provider is configured to request.
		env.Scope = p.record.OAuth.Scopes
	}
	if len(env.Extra) == 0 {
		env.Extra = nil
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return StoredCredential{}, fmt.Errorf("extidentities: marshal oauth envelope: %w", err)
	}
	return StoredCredential{
		Envelope:           string(raw),
		ExpiresAt:          env.Expiry,
		Access:             env.Scope,
		AccessFromProvider: grantedByProvider,
		Refreshable:        env.RefreshToken != "",
	}, nil
}

// RetrieveIdentity returns the access token only.
func (p *oauthProvider) RetrieveIdentity(_ context.Context, envelope string) (Credential, error) {
	env, err := decodeOAuthEnvelope(envelope)
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		Credentials: env.AccessToken,
		TokenType:   env.TokenType,
		Access:      env.Scope,
		ExpiresAt:   env.Expiry,
	}, nil
}

// Refresh exchanges the refresh token for a fresh access token. The stored
// token is presented as already-expired so the oauth2 TokenSource performs
// the refresh instead of handing back the still-valid token — the sweep
// refreshes ahead of expiry by design.
func (p *oauthProvider) Refresh(ctx context.Context, envelope string) (StoredCredential, bool, error) {
	env, err := decodeOAuthEnvelope(envelope)
	if err != nil {
		return StoredCredential{}, false, err
	}
	if env.RefreshToken == "" {
		return StoredCredential{}, false, nil
	}
	stale := &oauth2.Token{
		RefreshToken: env.RefreshToken,
		TokenType:    env.TokenType,
		Expiry:       time.Now().Add(-time.Minute),
	}
	fresh, err := p.cfg.TokenSource(p.ctxWithClient(ctx), stale).Token()
	if err != nil {
		return StoredCredential{}, false, exchangeError("token refresh", err)
	}
	// The library carries the previous refresh token forward when the
	// provider omits it; StoreIdentity's previous-envelope guard is the
	// backstop.
	out, err := p.StoreIdentity(ctx, tokenToPayload(fresh), envelope)
	if err != nil {
		return StoredCredential{}, false, err
	}
	return out, true, nil
}

// Revoke invalidates the stored tokens at the provider (RFC 7009), so
// disconnecting means disconnected rather than merely forgotten here.
//
// The refresh token goes first: revoking it ends the whole grant at most
// providers. The access token is revoked as well, because a provider that
// invalidates only the exact token named would otherwise leave a working
// access token in the wild until it expires. Per RFC 7009 an already-invalid
// token is answered with 200, so the second call is free when the first one
// cascaded.
func (p *oauthProvider) Revoke(ctx context.Context, envelope string) error {
	if p.record.OAuth.RevokeURL == "" {
		return ErrRevocationUnsupported
	}
	env, err := decodeOAuthEnvelope(envelope)
	if err != nil {
		return err
	}
	var firstErr error
	for _, target := range []struct{ token, hint string }{
		{env.RefreshToken, "refresh_token"},
		{env.AccessToken, "access_token"},
	} {
		if target.token == "" {
			continue
		}
		if err := p.revokeToken(ctx, target.token, target.hint); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// revokeToken posts one RFC 7009 revocation request.
func (p *oauthProvider) revokeToken(ctx context.Context, token, hint string) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}}
	basicAuth := p.cfg.Endpoint.AuthStyle != oauth2.AuthStyleInParams
	if !basicAuth {
		form.Set("client_id", p.cfg.ClientID)
		if p.cfg.ClientSecret != "" {
			form.Set("client_secret", p.cfg.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.record.OAuth.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("extidentities: build revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basicAuth {
		// Form-urlencode before base64, as RFC 6749 §2.3.1 requires and
		// x/oauth2 itself does at the token endpoint.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("extidentities: revocation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// RFC 7009: 200 covers "revoked" and "the token was already invalid" —
	// deliberately indistinguishable, so revocation cannot be used to test
	// whether a token exists.
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("extidentities: revocation rejected: %s (HTTP %d)", revocationErrorCode(resp), resp.StatusCode)
}

// revocationErrorCode extracts the OAuth error code from a failed
// revocation. Only the code: like exchangeError, the raw body is dropped
// because some providers echo the request — including the token — back in
// it (ADR-0008).
func revocationErrorCode(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body); err != nil || body.Error == "" {
		return "no error code"
	}
	return body.Error
}

// tokenToPayload converts an oauth2.Token into the map shape StoreIdentity
// accepts, preserving provider extras such as id_token.
func tokenToPayload(t *oauth2.Token) map[string]any {
	payload := map[string]any{"access_token": t.AccessToken}
	if t.RefreshToken != "" {
		payload["refresh_token"] = t.RefreshToken
	}
	if t.TokenType != "" {
		payload["token_type"] = t.TokenType
	}
	if !t.Expiry.IsZero() {
		payload["expiry"] = t.Expiry.UTC()
	}
	if scope, ok := t.Extra("scope").(string); ok && scope != "" {
		payload["scope"] = scope
	}
	for _, key := range []string{"id_token"} {
		if v := t.Extra(key); v != nil {
			payload[key] = v
		}
	}
	return payload
}

// exchangeError classifies a token-endpoint failure. Per ADR-0008 the
// wrapped message deliberately drops oauth2.RetrieveError's Error(), which
// embeds the provider's raw response body — some providers echo request
// parameters there.
func exchangeError(stage string, err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := re.ErrorCode
		if code == "" {
			code = fmt.Sprintf("http_%d", re.Response.StatusCode)
		}
		if code == "invalid_grant" {
			return &PermanentRefreshError{Reason: "provider rejected the grant (revoked or expired)"}
		}
		return fmt.Errorf("extidentities: %s failed: %s (HTTP %d)", stage, code, re.Response.StatusCode)
	}
	return fmt.Errorf("extidentities: %s failed", stage)
}

func decodeOAuthEnvelope(envelope string) (oauthEnvelope, error) {
	var env oauthEnvelope
	if err := json.Unmarshal([]byte(envelope), &env); err != nil {
		return oauthEnvelope{}, fmt.Errorf("extidentities: stored oauth envelope is unreadable: %w", err)
	}
	return env, nil
}

// toStringKeyedMap accepts the shapes a caller may send: a JSON object, an
// *oauth2.Token, or anything that marshals to an object.
func toStringKeyedMap(payload any) (map[string]any, error) {
	switch v := payload.(type) {
	case nil:
		return nil, fmt.Errorf("extidentities: credentials payload is empty")
	case map[string]any:
		return v, nil
	case *oauth2.Token:
		return tokenToPayload(v), nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("extidentities: credentials payload is not serializable: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("extidentities: credentials payload must be a JSON object")
	}
	return out, nil
}

// parseExpiry accepts RFC3339 strings, time.Time, and epoch seconds.
func parseExpiry(v any) *time.Time {
	switch t := v.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	case time.Time:
		utc := t.UTC()
		return &utc
	default:
		if secs, ok := toFloat(v); ok && secs > 0 {
			utc := time.Unix(int64(secs), 0).UTC()
			return &utc
		}
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// toStringSlice accepts a space-separated scope string or an array.
func toStringSlice(v any) []string {
	switch s := v.(type) {
	case string:
		return strings.Fields(s)
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
