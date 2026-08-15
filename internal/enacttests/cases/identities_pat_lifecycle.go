package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

const (
	patProviderName = "e2e-pat-provider"
	patTokenValue   = "ghp_e2e_secret_token_value"
)

// identitiesPATLifecycleCase covers the PAT half of the external identity
// service: register a provider, store a token, get it back, see it listed
// without its credentials, and delete it. It also proves the credential is
// unreadable at rest by asserting the listing never carries the token.
type identitiesPATLifecycleCase struct {
	utils.BaseCase
}

func NewIdentitiesPATLifecycle() utils.TestCase { return &identitiesPATLifecycleCase{} }

func (c *identitiesPATLifecycleCase) Name() string { return "TestIdentities_PATLifecycle" }

// identityURL builds a URL against the external identity service.
func (c *identitiesPATLifecycleCase) url(t *utils.T, path string) string {
	return t.Env.IdentitiesURL + path
}

func (c *identitiesPATLifecycleCase) Setup(t *utils.T) {
	// Leftovers from an aborted run must not fail registration.
	c.TearDown(t)
	body := fmt.Sprintf(`{"name":%q,"display_name":"E2E PAT","scheme":"bearer","header_name":"Authorization","docs_url":"https://example.test/docs","access_levels":{"read":["repo:status"],"admin":["repo:status","repo","admin:org"]}}`, patProviderName)
	status := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/providers/pat"), strings.NewReader(body), nil)
	if status != http.StatusCreated {
		t.Fatalf("register pat provider: got HTTP %d, want 201", status)
	}
}

func (c *identitiesPATLifecycleCase) Run(t *utils.T) {
	// Registering the same provider twice is a conflict, not a silent
	// overwrite — provider records carry client secrets elsewhere.
	dup := fmt.Sprintf(`{"name":%q,"display_name":"dup"}`, patProviderName)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/providers/pat"), strings.NewReader(dup), nil); st != http.StatusConflict {
		t.Errorf("duplicate provider registration: got HTTP %d, want 409", st)
	}

	// Storing against an unknown provider is rejected up front.
	unknown := `{"provider":"no-such-provider","credentials":"x"}`
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/identities"), strings.NewReader(unknown), nil); st != http.StatusBadRequest {
		t.Errorf("store for unknown provider: got HTTP %d, want 400", st)
	}

	// An unknown access level is rejected rather than silently ignored.
	badLevel := fmt.Sprintf(`{"provider":%q,"credentials":{"token":"x"},"access_level":"nope"}`, patProviderName)
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/identities"), strings.NewReader(badLevel), nil); st != http.StatusBadRequest {
		t.Errorf("store with an unknown access level: got HTTP %d, want 400", st)
	}

	// Store the token AS A LEVEL: the level's scopes are what gets recorded.
	storeBody := fmt.Sprintf(`{"provider":%q,"credentials":{"token":%q},"access_level":"admin"}`,
		patProviderName, patTokenValue)
	var stored struct {
		ID          string   `json:"id"`
		UserID      string   `json:"user_id"`
		Provider    string   `json:"provider"`
		Access      []string `json:"access"`
		AccessLevel string   `json:"access_level"`
		Refreshable bool     `json:"refreshable"`
		Status      string   `json:"status"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/identities"), strings.NewReader(storeBody), &stored); st != http.StatusCreated {
		t.Fatalf("store identity: got HTTP %d, want 201", st)
	}
	if stored.Refreshable {
		t.Errorf("a PAT identity is marked refreshable")
	}
	if stored.Status != "active" {
		t.Errorf("stored status = %q, want active", stored.Status)
	}
	// The named level was expanded into its concrete scopes and recorded as
	// the level, so a UI can say "admin" while storage stays honest.
	if stored.AccessLevel != "admin" {
		t.Errorf("stored access_level = %q, want admin", stored.AccessLevel)
	}
	if !contains(stored.Access, "repo") || !contains(stored.Access, "admin:org") {
		t.Errorf("stored access = %v, want the admin level's scopes", stored.Access)
	}

	// The listing exposes metadata but never the credential.
	var listing struct {
		Identities []struct {
			Provider string `json:"provider"`
		} `json:"identities"`
	}
	_, rawList := t.DoJSONRaw("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, "/v1/identities?provider="+patProviderName), nil, &listing)
	if len(listing.Identities) != 1 {
		t.Fatalf("listing returned %d identities, want 1", len(listing.Identities))
	}
	if strings.Contains(rawList, patTokenValue) {
		t.Errorf("the identity listing leaks the stored token")
	}

	// Retrieval returns the token itself.
	var cred struct {
		Credentials string   `json:"credentials"`
		TokenType   string   `json:"token_type"`
		Access      []string `json:"access"`
		AccessLevel string   `json:"access_level"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/identities/credentials?provider=%s", patProviderName)), nil, &cred); st != http.StatusOK {
		t.Fatalf("retrieve credentials: got HTTP %d, want 200", st)
	}
	if cred.Credentials != patTokenValue {
		t.Errorf("retrieved credential %q, want the stored token", cred.Credentials)
	}
	if cred.TokenType != "bearer" {
		t.Errorf("token_type = %q, want bearer (the provider's scheme)", cred.TokenType)
	}
	if cred.AccessLevel != "admin" {
		t.Errorf("retrieved access_level = %q, want admin", cred.AccessLevel)
	}

	// required_access speaks either vocabulary. A level is satisfied by
	// concrete scope coverage, not by a hierarchy the platform invents —
	// "admin" only covers "read" because its definition lists read's
	// scopes.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/identities/credentials?provider=%s&required_access=read", patProviderName)), nil, nil); st != http.StatusOK {
		t.Errorf("retrieval requiring a covered level: got HTTP %d, want 200", st)
	}
	// ...and a raw scope it does not cover is refused.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/identities/credentials?provider=%s&required_access=gist", patProviderName)), nil, nil); st != http.StatusConflict {
		t.Errorf("retrieval with unmet required_access: got HTTP %d, want 409", st)
	}

	// A provider with identities cannot be deleted by accident.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+patProviderName), nil, nil); st != http.StatusConflict {
		t.Errorf("deleting a referenced provider: got HTTP %d, want 409", st)
	}

	// Delete the identity; retrieval then 404s.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, fmt.Sprintf("/v1/identities?provider=%s", patProviderName)), nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete identity: got HTTP %d, want 204", st)
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, fmt.Sprintf("/v1/identities/credentials?provider=%s", patProviderName)), nil, nil); st != http.StatusNotFound {
		t.Errorf("retrieval after delete: got HTTP %d, want 404", st)
	}

	// Forcing the deletion takes the stored credentials with it. Leaving them
	// behind would keep a secret nobody can ever open again — the provider
	// record is what knows how to parse the envelope.
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodPost,
		c.url(t, "/v1/identities"), strings.NewReader(storeBody), nil); st != http.StatusCreated {
		t.Fatalf("re-store identity before the forced delete: got HTTP %d, want 201", st)
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+patProviderName+"?force=true"), nil, nil); st != http.StatusNoContent {
		t.Fatalf("forced provider delete: got HTTP %d, want 204", st)
	}
	var afterForce struct {
		Identities []struct {
			Provider string `json:"provider"`
		} `json:"identities"`
	}
	if st := t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodGet,
		c.url(t, "/v1/identities?provider="+patProviderName), nil, &afterForce); st != http.StatusOK {
		t.Fatalf("listing after the forced delete: got HTTP %d, want 200", st)
	}
	if len(afterForce.Identities) != 0 {
		t.Errorf("%d identities survived the forced provider delete, want 0 — they are unreadable orphans",
			len(afterForce.Identities))
	}
}

func (c *identitiesPATLifecycleCase) TearDown(t *utils.T) {
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, fmt.Sprintf("/v1/identities?provider=%s", patProviderName)), nil, nil)
	t.DoJSON("enact-main", utils.IdentitiesAudience, http.MethodDelete,
		c.url(t, "/v1/providers/"+patProviderName+"?force=true"), nil, nil)
}

// contains reports whether haystack holds needle.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
