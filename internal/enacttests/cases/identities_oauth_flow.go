package cases

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enact/internal/enacttests/utils"
)

const (
	oauthProviderName = "e2e-oauth-provider"
)

// identitiesOAuthFlowCase drives the whole OAuth path against the fixture
// authorization server embedded in this service: register the provider,
// start an authorization, follow the consent redirect, land on the
// callback, and read back the access token. It then waits for the refresh
// sweep and asserts the stored access token was rotated — the only way to
// prove the background refresh actually runs.
type identitiesOAuthFlowCase struct {
	utils.BaseCase
}

func NewIdentitiesOAuthFlow() utils.TestCase { return &identitiesOAuthFlowCase{} }

func (c *identitiesOAuthFlowCase) Name() string { return "TestIdentities_OAuthFlowAndRefresh" }

func (c *identitiesOAuthFlowCase) url(t *utils.T, path string) string {
	return t.Env.IdentitiesURL + path
}

func (c *identitiesOAuthFlowCase) credentialsURL(t *utils.T) string {
	return c.url(t, "/v1/identities/credentials?provider="+oauthProviderName)
}

func (c *identitiesOAuthFlowCase) Setup(t *utils.T) {
	c.TearDown(t)
	body := fmt.Sprintf(`{"name":%q,"display_name":"E2E OAuth","authorize_url":"%s/authorize","token_url":"%s/token","revoke_url":"%s/revoke","client_id":%q,"client_secret":%q,"scopes":["read","write"],"use_pkce":true,"access_type":"offline","auth_style":"params","access_levels":{"basic":["read"],"full":["read","write"],"impossible":["read","write","never-granted"]}}`,
		oauthProviderName, t.Env.OAuthFixtureURL, t.Env.OAuthFixtureURL, t.Env.OAuthFixtureURL,
		utils.FixtureClientID, utils.FixtureClientSecret)
	var registered struct {
		ClientSecretSet bool   `json:"client_secret_set"`
		AuthorizeURL    string `json:"authorize_url"`
	}
	status, raw := t.DoJSONRaw("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/providers/oauth"), strings.NewReader(body), &registered)
	if status != http.StatusCreated {
		t.Fatalf("register oauth provider: got HTTP %d, want 201", status)
	}
	if !registered.ClientSecretSet {
		t.Errorf("registered provider does not report a stored client secret")
	}
	// The client secret is stored sealed and must never come back out.
	if strings.Contains(raw, utils.FixtureClientSecret) {
		t.Errorf("the provider response echoes the client secret")
	}
}

func (c *identitiesOAuthFlowCase) Run(t *utils.T) {
	// Begin the authorization: the service returns the provider's consent
	// URL plus the state correlating the eventual callback.
	var start struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	// Passing a level and raw scopes at once is ambiguous and refused.
	both := c.url(t, fmt.Sprintf("/v1/oauth/authorize?provider=%s&access_level=basic&scopes=read", oauthProviderName))
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, both, nil, nil); st != http.StatusBadRequest {
		t.Errorf("authorize with both access_level and scopes: got HTTP %d, want 400", st)
	}
	// Naming neither is refused: a consent screen must never ask for
	// permissions the caller did not state.
	neither := c.url(t, "/v1/oauth/authorize?provider="+oauthProviderName)
	var refusal struct {
		Error string `json:"error"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, neither, nil, &refusal); st != http.StatusBadRequest {
		t.Errorf("authorize with neither access_level nor scopes: got HTTP %d, want 400", st)
	}
	// The refusal names what may be picked, so a UI can act on it.
	if !strings.Contains(refusal.Error, "basic") || !strings.Contains(refusal.Error, "full") {
		t.Errorf("refusal %q does not list the provider's access levels", refusal.Error)
	}

	// An OAuth provider must state what can be asked for: consent is a list
	// of permissions, so one with neither scopes nor access levels describes
	// nothing a caller could request.
	bare := fmt.Sprintf(`{"name":"%s-bare","authorize_url":"%s/authorize","token_url":"%s/token","client_id":%q,"client_secret":%q}`,
		oauthProviderName, t.Env.OAuthFixtureURL, t.Env.OAuthFixtureURL, utils.FixtureClientID, utils.FixtureClientSecret)
	var bareErr struct {
		Error string `json:"error"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/providers/oauth"), strings.NewReader(bare), &bareErr); st != http.StatusBadRequest {
		t.Errorf("registering an oauth provider with no scopes and no access levels: got HTTP %d, want 400", st)
		t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
			c.url(t, "/v1/providers/"+oauthProviderName+"-bare?force=true"), nil, nil)
	} else if !strings.Contains(bareErr.Error, "scopes") {
		t.Errorf("the refusal %q does not say what is missing", bareErr.Error)
	}
	// Levels alone are enough: they name the scopes they stand for.
	levelsOnly := fmt.Sprintf(`{"name":"%s-levels","authorize_url":"%s/authorize","token_url":"%s/token","client_id":%q,"client_secret":%q,"access_levels":{"basic":["read"]}}`,
		oauthProviderName, t.Env.OAuthFixtureURL, t.Env.OAuthFixtureURL, utils.FixtureClientID, utils.FixtureClientSecret)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/providers/oauth"), strings.NewReader(levelsOnly), nil); st != http.StatusCreated {
		t.Errorf("registering an oauth provider with access levels but no scopes: got HTTP %d, want 201", st)
	}
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+oauthProviderName+"-levels?force=true"), nil, nil)

	// An undefined level is refused rather than silently ignored.
	unknown := c.url(t, fmt.Sprintf("/v1/oauth/authorize?provider=%s&access_level=nope", oauthProviderName))
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, unknown, nil, nil); st != http.StatusNotFound && st != http.StatusBadRequest {
		t.Errorf("authorize with an unknown access level: got HTTP %d, want 400", st)
	}

	// Authorize by LEVEL: the consent URL must request the level's scopes.
	authorizeURL := c.url(t, fmt.Sprintf("/v1/oauth/authorize?provider=%s&access_level=full", oauthProviderName))
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, authorizeURL, nil, &start); st != http.StatusOK {
		t.Fatalf("authorize: got HTTP %d, want 200", st)
	}
	if start.State == "" || start.AuthorizationURL == "" {
		t.Fatalf("authorize returned an empty state or URL")
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("authorization_url is not a URL: %v", err)
	}
	q := parsed.Query()
	// PKCE and offline access must reach the provider, or no refresh token
	// is ever issued.
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("consent URL lacks a PKCE challenge: %v", q)
	}
	if q.Get("access_type") != "offline" {
		t.Errorf("consent URL access_type = %q, want offline", q.Get("access_type"))
	}
	if scope := q.Get("scope"); !strings.Contains(scope, "read") || !strings.Contains(scope, "write") {
		t.Errorf("consent URL scope = %q, want the \"full\" level's scopes", scope)
	}

	// A browser would now follow the consent URL; the fixture immediately
	// redirects back to the service's callback, which finishes the flow.
	// Only the callback's outcome matters, so follow redirects.
	status := t.DoPlain(http.MethodGet, start.AuthorizationURL)
	if status != http.StatusOK {
		t.Fatalf("consent + callback round trip: got HTTP %d, want 200", status)
	}

	// The credential is now retrievable, and it is an access token issued
	// by the fixture.
	var cred struct {
		Credentials string   `json:"credentials"`
		TokenType   string   `json:"token_type"`
		Access      []string `json:"access"`
		AccessLevel string   `json:"access_level"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, &cred); st != http.StatusOK {
		t.Fatalf("retrieve oauth credentials: got HTTP %d, want 200", st)
	}
	if !strings.HasPrefix(cred.Credentials, "fixture-access-") {
		t.Fatalf("retrieved credential %q, want a fixture access token", cred.Credentials)
	}
	if len(cred.Access) == 0 {
		t.Errorf("retrieved credential carries no scopes")
	}
	// The fixture grants exactly the "full" level, so the label sticks.
	if cred.AccessLevel != "full" {
		t.Errorf("retrieved access_level = %q, want full", cred.AccessLevel)
	}
	// The recorded access is what the PROVIDER granted, and required_access
	// checks answer against that — not against what was requested.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.credentialsURL(t)+"&required_access=impossible", nil, nil); st != http.StatusConflict {
		t.Errorf("requiring a level the provider never granted: got HTTP %d, want 409", st)
	}
	// The refresh token stays inside the service: retrieval returns the
	// access token only.
	_, rawCred := t.DoJSONRaw("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, nil)
	if strings.Contains(rawCred, "fixture-refresh-") {
		t.Errorf("the retrieved credential leaks the refresh token")
	}

	// The identity is marked refreshable and expiring, which is what puts
	// it in the sweep's work list.
	var listing struct {
		Identities []struct {
			Refreshable bool   `json:"refreshable"`
			Status      string `json:"status"`
			ExpiresAt   string `json:"expires_at"`
		} `json:"identities"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, "/v1/identities?provider="+oauthProviderName), nil, &listing); st != http.StatusOK {
		t.Fatalf("list identities: got HTTP %d, want 200", st)
	}
	if len(listing.Identities) != 1 || !listing.Identities[0].Refreshable || listing.Identities[0].ExpiresAt == "" {
		t.Fatalf("stored identity is not refreshable/expiring: %+v", listing.Identities)
	}

	// The fixture issues 60-second tokens, so every sweep finds this one
	// inside the refresh window. Poll until the access token changes.
	original := cred.Credentials
	t.Eventually(90*time.Second, "the refresh sweep rotates the access token", func() (bool, string) {
		var current struct {
			Credentials string `json:"credentials"`
		}
		if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, &current); st != http.StatusOK {
			return false, fmt.Sprintf("retrieval returned HTTP %d", st)
		}
		if current.Credentials == original {
			return false, "the stored access token is still the original one (is IDENTITIES_REFRESH_AT longer than this window?)"
		}
		if !strings.HasPrefix(current.Credentials, "fixture-access-") {
			return false, "the rotated credential is not a fixture access token"
		}
		return true, ""
	})

	// Re-connecting must never NARROW the credential. One identity serves
	// every consumer now, so a second authorization at the "basic" level asks
	// for what the user already granted as well — otherwise connecting from
	// one server's prompt would strip the access another server depends on.
	var second struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/oauth/authorize?provider=%s&access_level=basic", oauthProviderName)), nil, &second); st != http.StatusOK {
		t.Fatalf("re-authorize at the narrower level: got HTTP %d, want 200", st)
	}
	reparsed, err := url.Parse(second.AuthorizationURL)
	if err != nil {
		t.Fatalf("second authorization_url is not a URL: %v", err)
	}
	if scope := reparsed.Query().Get("scope"); !strings.Contains(scope, "write") {
		t.Errorf("re-authorization scope = %q, want the already-granted \"write\" carried over", scope)
	}
	if st := t.DoPlain(http.MethodGet, second.AuthorizationURL); st != http.StatusOK {
		t.Fatalf("second consent + callback round trip: got HTTP %d, want 200", st)
	}
	// The label follows what was asked for, but coverage is by scope: the
	// credential still satisfies the wider level.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.credentialsURL(t)+"&required_access=full", nil, nil); st != http.StatusOK {
		t.Errorf("after re-connecting at \"basic\": got HTTP %d, want 200 — the re-authorization downgraded the credential", st)
	}

	// Disconnecting must END the access, not merely forget it: the platform
	// revokes at the provider before deleting its own copy (RFC 7009).
	var live struct {
		Credentials string `json:"credentials"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, &live); st != http.StatusOK {
		t.Fatalf("retrieve the credential before disconnecting: got HTTP %d, want 200", st)
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/identities?provider="+oauthProviderName), nil, nil); st != http.StatusNoContent {
		t.Fatalf("disconnect: got HTTP %d, want 204", st)
	}
	revoked, hint := c.revocation(t, live.Credentials)
	if !revoked {
		t.Errorf("the provider never saw a revocation for the disconnected access token — disconnecting only deleted the local copy")
	}
	if hint != "access_token" {
		t.Errorf("access token revoked with token_type_hint %q, want access_token", hint)
	}
	// And the credential is gone from the platform.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, nil); st != http.StatusNotFound {
		t.Errorf("retrieval after disconnect: got HTTP %d, want 404", st)
	}

	// Force-deleting the PROVIDER destroys other people's credentials, so it
	// must revoke them too — and it has to happen before the provider record
	// goes, since that record holds the revocation endpoint and the client
	// secret. Connect once more, then delete the provider out from under it.
	c.connect(t, "full")
	var doomed struct {
		Credentials string `json:"credentials"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet, c.credentialsURL(t), nil, &doomed); st != http.StatusOK {
		t.Fatalf("retrieve the credential before the forced provider delete: got HTTP %d, want 200", st)
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+oauthProviderName+"?force=true"), nil, nil); st != http.StatusNoContent {
		t.Fatalf("forced provider delete: got HTTP %d, want 204", st)
	}
	if revoked, _ := c.revocation(t, doomed.Credentials); !revoked {
		t.Errorf("the forced provider delete cascaded the identity away without revoking it; the grant is still live at the provider")
	}
}

// revocation asks the fixture whether a token was revoked, and with which
// token_type_hint.
func (c *identitiesOAuthFlowCase) revocation(t *utils.T, token string) (bool, string) {
	var out struct {
		Revoked bool   `json:"revoked"`
		Hint    string `json:"token_type_hint"`
	}
	st := t.DoPlainJSON(http.MethodGet, t.Env.OAuthFixtureURL+"/revoked?token="+url.QueryEscape(token), &out)
	if st != http.StatusOK && st != http.StatusNotFound {
		t.Fatalf("fixture revocation lookup: got HTTP %d, want 200 or 404", st)
	}
	return out.Revoked, out.Hint
}

// connect drives one whole authorize → consent → callback round trip at the
// named access level.
func (c *identitiesOAuthFlowCase) connect(t *utils.T, accessLevel string) {
	var start struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/oauth/authorize?provider=%s&access_level=%s", oauthProviderName, accessLevel)), nil, &start); st != http.StatusOK {
		t.Fatalf("authorize at %q: got HTTP %d, want 200", accessLevel, st)
	}
	if st := t.DoPlain(http.MethodGet, start.AuthorizationURL); st != http.StatusOK {
		t.Fatalf("consent + callback round trip at %q: got HTTP %d, want 200", accessLevel, st)
	}
}

func (c *identitiesOAuthFlowCase) TearDown(t *utils.T) {
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/identities?provider="+oauthProviderName), nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+oauthProviderName+"?force=true"), nil, nil)
	for _, suffix := range []string{"-bare", "-levels"} {
		t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
			c.url(t, "/v1/providers/"+oauthProviderName+suffix+"?force=true"), nil, nil)
	}
}
