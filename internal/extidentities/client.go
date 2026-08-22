package extidentities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling the enact-external-identities
// service.
type ClientConfig struct {
	BaseURL string        `env:"EXTERNAL_IDENTITIES_API_URL, default=http://localhost:8008"`
	Timeout time.Duration `env:"EXTERNAL_IDENTITIES_API_TIMEOUT, default=10s"`
}

// Client is the HTTP wrapper other services use to reach the external
// identity service — the source of truth for third-party credentials. It
// lives in the domain package so callers depend on the domain, never on
// another service's internals.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the service at cfg.BaseURL. base is the
// caller's S2S signing transport (nil = http.DefaultTransport); the tracing
// transport is layered on top.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	return &Client{
		http:    &http.Client{Transport: requesthelper.NewTransport(base), Timeout: cfg.Timeout},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// BaseURL is the service root, used when a caller must build a browser-
// facing redirect into the OAuth flow.
func (c *Client) BaseURL() string { return c.baseURL }

// StoreIdentityRequest stores a credential the caller already holds (the
// PAT path, or a tuple obtained elsewhere).
type StoreIdentityRequest struct {
	Provider    string   `json:"provider"`
	Credentials any      `json:"credentials"`
	Access      []string `json:"access,omitempty"`
	// AccessLevel names a provider-defined level instead of raw scopes.
	AccessLevel string `json:"access_level,omitempty"`
}

// IdentitySummary is an identity without its credentials — what listings
// and stores return.
type IdentitySummary struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	Provider     string     `json:"provider"`
	ProviderType string     `json:"provider_type"`
	Access       []string   `json:"access,omitempty"`
	AccessLevel  string     `json:"access_level,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Refreshable  bool       `json:"refreshable"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ProviderSummary describes a registered provider; the client secret is
// never part of it.
type ProviderSummary struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	DisplayName     string `json:"display_name,omitempty"`
	AuthorizeURL    string `json:"authorize_url,omitempty"`
	TokenURL        string `json:"token_url,omitempty"`
	ClientID        string `json:"client_id,omitempty"`
	ClientSecretSet bool   `json:"client_secret_set,omitempty"`
	// CallbackURL is the redirect URI to register in the provider's console.
	CallbackURL string   `json:"callback_url,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	Scheme      string   `json:"scheme,omitempty"`
	DocsURL     string   `json:"docs_url,omitempty"`
	// AccessLevels are the provider's named tiers and the scopes each
	// needs — the vocabulary a UI offers instead of raw scopes.
	AccessLevels map[string][]string `json:"access_levels,omitempty"`
}

// RegisterOAuthProviderRequest registers an OAuth provider: either a
// discovery URL (the well-known document, whose endpoints are fetched) or
// explicit endpoints, plus this platform's client registration.
type RegisterOAuthProviderRequest struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name,omitempty"`
	DiscoveryURL string   `json:"discovery_url,omitempty"`
	Issuer       string   `json:"issuer,omitempty"`
	AuthorizeURL string   `json:"authorize_url,omitempty"`
	TokenURL     string   `json:"token_url,omitempty"`
	RevokeURL    string   `json:"revoke_url,omitempty"`
	UserInfoURL  string   `json:"userinfo_url,omitempty"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	UsePKCE      bool     `json:"use_pkce,omitempty"`
	AccessType   string   `json:"access_type,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	AuthStyle    string   `json:"auth_style,omitempty"`

	ExtraAuthParams map[string]string   `json:"extra_auth_params,omitempty"`
	AccessLevels    map[string][]string `json:"access_levels,omitempty"`
}

// RegisterPATProviderRequest registers a provider whose credentials users
// paste in. Beyond a name it needs nothing: there is no client
// registration, no endpoints and no consent flow — only presentation
// metadata and, optionally, the access levels a token may be labelled with.
type RegisterPATProviderRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
	HeaderName  string `json:"header_name,omitempty"`
	DocsURL     string `json:"docs_url,omitempty"`

	AccessLevels map[string][]string `json:"access_levels,omitempty"`
}

// AuthorizationStart is the consent URL a browser must visit, plus the
// opaque state correlating the eventual callback.
type AuthorizationStart struct {
	AuthorizationURL string    `json:"authorization_url"`
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// ConflictError is a 409 from the identity service: the stored credential
// exists but does not satisfy the request (revoked, or the grant does not
// cover the required access). It is distinct from BadRequestError because
// only a conflict is fixable by the USER re-authorizing — a caller that
// waits for a credential must not wait on a malformed request or an
// outage.
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

// ProviderGoneError is a 424 from the identity service: a credential is
// stored but its provider is no longer registered, so nothing can open it.
// Only an administrator can fix this, which is why it is not a
// ConflictError — a caller must fail fast rather than wait for a user who
// has no provider left to connect to.
type ProviderGoneError struct{ Message string }

func (e *ProviderGoneError) Error() string { return e.Message }

// do issues one JSON request. 404 maps to found=false; 409 to
// *ConflictError; 424 to *ProviderGoneError; 400/502 to
// *requesthelper.BadRequestError.
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, fmt.Errorf("extidentities: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("extidentities: %s %s: %w", method, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case wantStatus:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return false, fmt.Errorf("extidentities: decode response: %w", err)
			}
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusBadRequest, http.StatusForbidden, http.StatusConflict,
		http.StatusFailedDependency, http.StatusBadGateway:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = http.StatusText(resp.StatusCode)
		}
		switch resp.StatusCode {
		case http.StatusConflict:
			return false, &ConflictError{Message: apiErr.Error}
		case http.StatusFailedDependency:
			return false, &ProviderGoneError{Message: apiErr.Error}
		case http.StatusForbidden:
			// An authorization refusal, which the caller must relay as 403
			// naming the permission. Folded into the generic branch it would
			// surface as a 502 and read as an outage.
			return false, &requesthelper.ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("extidentities: %s %s: unexpected status %d", method, endpoint, resp.StatusCode)
	}
}

// Store persists a credential for the calling user (from ctx).
func (c *Client) Store(ctx context.Context, body StoreIdentityRequest) (IdentitySummary, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return IdentitySummary{}, fmt.Errorf("extidentities: marshal store request: %w", err)
	}
	var out IdentitySummary
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/identities", payload, http.StatusCreated, &out); err != nil {
		return IdentitySummary{}, err
	}
	return out, nil
}

// List returns the calling user's identities, optionally narrowed.
func (c *Client) List(ctx context.Context, provider string) ([]IdentitySummary, error) {
	params := url.Values{}
	if provider != "" {
		params.Set("provider", provider)
	}
	endpoint := c.baseURL + "/v1/identities"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	var out struct {
		Identities []IdentitySummary `json:"identities"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Identities, nil
}

// Credentials fetches the usable credential for (calling user, provider).
// requiredAccess, when set, makes the service refuse an identity whose
// grant does not cover it. The boolean reports existence.
func (c *Client) Credentials(ctx context.Context, provider string, requiredAccess []string) (Credential, bool, error) {
	params := url.Values{}
	params.Set("provider", provider)
	for _, a := range requiredAccess {
		params.Add("required_access", a)
	}
	var out Credential
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/identities/credentials?"+params.Encode(), nil, http.StatusOK, &out)
	if err != nil {
		return Credential{}, false, err
	}
	return out, found, nil
}

// Delete removes the calling user's identity at a provider. The boolean
// reports existence.
func (c *Client) Delete(ctx context.Context, provider string) (bool, error) {
	params := url.Values{}
	params.Set("provider", provider)
	return c.do(ctx, http.MethodDelete, c.baseURL+"/v1/identities?"+params.Encode(), nil, http.StatusNoContent, nil)
}

// DeletedIdentities reports the outcome of a bulk delete: what the platform
// forgot, and how much of it was also revoked at the provider.
type DeletedIdentities struct {
	Deleted               int `json:"deleted"`
	Revoked               int `json:"revoked"`
	RevocationUnsupported int `json:"revocation_unsupported"`
	RevocationFailed      int `json:"revocation_failed"`
}

// DeleteAll removes every identity of the user in ctx, revoking each at its
// provider first. For account deletion: credentials must not outlive the
// account that authorized them.
func (c *Client) DeleteAll(ctx context.Context) (DeletedIdentities, error) {
	var out DeletedIdentities
	if _, err := c.do(ctx, http.MethodDelete, c.baseURL+"/v1/identities/all", nil, http.StatusOK, &out); err != nil {
		return DeletedIdentities{}, err
	}
	return out, nil
}

// RegisterOAuthProvider registers an OAuth identity provider.
func (c *Client) RegisterOAuthProvider(ctx context.Context, body RegisterOAuthProviderRequest) (ProviderSummary, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return ProviderSummary{}, fmt.Errorf("extidentities: marshal provider request: %w", err)
	}
	var out ProviderSummary
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/providers/oauth", payload, http.StatusCreated, &out); err != nil {
		return ProviderSummary{}, err
	}
	return out, nil
}

// RegisterPATProvider registers a personal-access-token provider.
func (c *Client) RegisterPATProvider(ctx context.Context, body RegisterPATProviderRequest) (ProviderSummary, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return ProviderSummary{}, fmt.Errorf("extidentities: marshal provider request: %w", err)
	}
	var out ProviderSummary
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/providers/pat", payload, http.StatusCreated, &out); err != nil {
		return ProviderSummary{}, err
	}
	return out, nil
}

// DeleteProvider removes a provider. Without force the service refuses
// while stored identities still reference it; with force those identities
// are deleted along with it. The boolean reports existence.
func (c *Client) DeleteProvider(ctx context.Context, name string) (bool, error) {
	endpoint := c.baseURL + "/v1/providers/" + url.PathEscape(name)
	return c.do(ctx, http.MethodDelete, endpoint, nil, http.StatusNoContent, nil)
}

// Providers lists the registered identity providers.
func (c *Client) Providers(ctx context.Context) ([]ProviderSummary, error) {
	var out struct {
		Providers []ProviderSummary `json:"providers"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/providers", nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

// Authorize starts an OAuth flow for the calling user and returns the
// consent URL to send the browser to. redirectURI is where the service
// sends the browser once the callback completes.
func (c *Client) Authorize(ctx context.Context, provider, redirectURI string, scopes []string, accessLevel string) (AuthorizationStart, bool, error) {
	params := url.Values{}
	params.Set("provider", provider)
	if redirectURI != "" {
		params.Set("redirect_uri", redirectURI)
	}
	for _, s := range scopes {
		params.Add("scopes", s)
	}
	if accessLevel != "" {
		params.Set("access_level", accessLevel)
	}
	var out AuthorizationStart
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/oauth/authorize?"+params.Encode(), nil, http.StatusOK, &out)
	if err != nil {
		return AuthorizationStart{}, false, err
	}
	return out, found, nil
}
