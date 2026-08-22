package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// mainAPIKeyAccessCase covers programmatic access: minting a key, using it in
// place of a session, the surfaces it must not reach, and revocation.
//
// The account is the same fixture the session-lifecycle case uses, so this
// exercises a key belonging to a real, organization-placed user rather than a
// specially privileged one.
type mainAPIKeyAccessCase struct {
	session *utils.MainSession
	keyID   string
	key     string
}

func NewMainAPIKeyAccess() utils.TestCase { return &mainAPIKeyAccessCase{} }

func (c *mainAPIKeyAccessCase) Name() string { return "TestMainAuth_APIKeyAccess" }

type apiKeyDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	Prefix     string `json:"prefix"`
	KeyHash    string `json:"key_hash"`
	LastUsedAt string `json:"last_used_at"`
	Error      string `json:"error"`
}

func (c *mainAPIKeyAccessCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	var created apiKeyDTO
	if st := c.session.DoJSON(t, http.MethodPost, "/auth/keys",
		strings.NewReader(`{"name":"e2e workflow"}`), &created); st != http.StatusCreated {
		t.Fatalf("create api key: got HTTP %d (%s), want 201", st, created.Error)
	}
	if created.Key == "" || created.ID == "" {
		t.Fatalf("create api key: response carried no key or id")
	}
	c.keyID, c.key = created.ID, created.Key
}

func (c *mainAPIKeyAccessCase) Run(t *utils.T) {
	// The key is shaped as advertised, and its prefix identifies it without
	// being enough to reconstruct it.
	if !strings.HasPrefix(c.key, "enact_sk_") {
		t.Errorf("api key %q does not carry the enact_sk_ prefix", c.key[:min(12, len(c.key))])
	}

	// Listing shows the key's metadata and NEVER anything that authenticates.
	var listing struct {
		Keys []apiKeyDTO `json:"keys"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/auth/keys", nil, &listing); st != http.StatusOK {
		t.Fatalf("list api keys: got HTTP %d, want 200", st)
	}
	found := false
	for _, k := range listing.Keys {
		if k.ID != c.keyID {
			continue
		}
		found = true
		if k.Key != "" || k.KeyHash != "" {
			t.Errorf("listed key exposes a credential: key=%q key_hash=%q", k.Key, k.KeyHash)
		}
		if !strings.HasPrefix(c.key, k.Prefix) {
			t.Errorf("listed prefix %q is not a prefix of the issued key", k.Prefix)
		}
	}
	if !found {
		t.Fatalf("the created key is not in the listing")
	}

	// The key authenticates the surfaces a workflow needs.
	for _, path := range []string{"/agents", "/knowledge-bases", "/mcp-servers", "/models", "/conversations"} {
		if st := t.DoAPIKey(c.key, http.MethodGet, path, nil, nil, nil); st != http.StatusOK {
			t.Errorf("key -> GET %s: got HTTP %d, want 200", path, st)
		}
	}

	// /auth/me answers for a key — how a caller checks that a key is live and
	// learns whose it is — and answers as the KEY'S OWNER, not as anyone else.
	// It is the one route under /auth that admits a key, because it reads
	// identity rather than changing credentials; /auth/keys next to it must
	// still refuse one, so the two cannot quietly drift together.
	var whoami struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Error string `json:"error"`
	}
	if st := t.DoAPIKey(c.key, http.MethodGet, "/auth/me", nil, &whoami, nil); st != http.StatusOK {
		t.Errorf("key -> GET /auth/me: got HTTP %d (%s), want 200", st, whoami.Error)
	} else if whoami.Email != mainTestEmail {
		t.Errorf("key -> /auth/me: email %q, want the key owner %q", whoami.Email, mainTestEmail)
	}
	// And a key that has been presented with a forged identity still answers
	// for its owner.
	var forged struct {
		Email string `json:"email"`
	}
	t.DoAPIKey(c.key, http.MethodGet, "/auth/me", nil, &forged, map[string]string{"X-User-Id": "some-other-user"})
	if forged.Email != mainTestEmail {
		t.Errorf("key + forged X-User-Id -> /auth/me: email %q, want %q", forged.Email, mainTestEmail)
	}

	// ...and does not reach the ones it is excluded from. These would be a
	// privilege escalation, not merely an inconvenience.
	//
	// Each is also called WITH the session, because a 401 from a route that
	// does not exist proves nothing — a mistyped path would look like a
	// perfect exclusion. Requiring the session to succeed is what makes the
	// refusal attributable to the key. (/admin/users is exempt: the fixture
	// account is not the administrator, so a session gets 403 there.)
	for _, path := range []string{"/organizations/me", "/identities", "/auth/keys"} {
		if st := t.DoAPIKey(c.key, http.MethodGet, path, nil, nil, nil); st != http.StatusUnauthorized {
			t.Errorf("key -> GET %s: got HTTP %d, want 401 (excluded surface)", path, st)
		}
		if st := c.session.DoJSON(t, http.MethodGet, path, nil, nil); st != http.StatusOK {
			t.Errorf("session -> GET %s: got HTTP %d, want 200 — the exclusion check above is vacuous unless this route exists", path, st)
		}
	}
	if st := t.DoAPIKey(c.key, http.MethodGet, "/admin/users", nil, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("key -> GET /admin/users: got HTTP %d, want 401", st)
	}
	// Specifically: a key cannot mint another key, which is what would make
	// revoking the first one pointless.
	if st := t.DoAPIKey(c.key, http.MethodPost, "/auth/keys",
		strings.NewReader(`{"name":"escalation"}`), nil, nil); st != http.StatusUnauthorized {
		t.Errorf("key -> POST /auth/keys: got HTTP %d, want 401", st)
	}

	// A valid key plus a forged identity header must still be the key's owner.
	// The header is honoured by the container-wide identity filter, so the
	// only thing standing between it and a cross-user read is requireCaller
	// overwriting the context.
	var impersonated struct {
		Agents []struct {
			UserID string `json:"user_id"`
		} `json:"agents"`
	}
	st := t.DoAPIKey(c.key, http.MethodGet, "/agents", nil, &impersonated,
		map[string]string{"X-User-Id": "some-other-user"})
	if st != http.StatusOK {
		t.Fatalf("key + forged X-User-Id -> /agents: got HTTP %d, want 200", st)
	}
	for _, a := range impersonated.Agents {
		if a.UserID == "some-other-user" {
			t.Errorf("key + forged X-User-Id returned another user's agent (%s)", a.UserID)
		}
	}

	// Nonsense keys are refused, and so is the empty one.
	if st := t.DoAPIKey("enact_sk_notarealkey000000000000000000000000", http.MethodGet, "/agents", nil, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("unknown key -> /agents: got HTTP %d, want 401", st)
	}
	if st := t.DoAPIKey("", http.MethodGet, "/agents", nil, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("no credentials -> /agents: got HTTP %d, want 401", st)
	}

	// Revocation takes effect immediately, not after the cache TTL.
	if st := c.session.DoJSON(t, http.MethodDelete, "/auth/keys/"+c.keyID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("revoke api key: got HTTP %d, want 204", st)
	}
	if st := t.DoAPIKey(c.key, http.MethodGet, "/agents", nil, nil, nil); st != http.StatusUnauthorized {
		t.Errorf("revoked key -> /agents: got HTTP %d, want 401", st)
	}
	c.key = ""

	// Revoking an id the account does not have is a 404, not a silent success.
	if st := c.session.DoJSON(t, http.MethodDelete, "/auth/keys/"+c.keyID, nil, nil); st != http.StatusNotFound {
		t.Errorf("re-revoke: got HTTP %d, want 404", st)
	}
	c.keyID = ""
}

func (c *mainAPIKeyAccessCase) TearDown(t *utils.T) {
	if c.keyID == "" || c.session == nil {
		return
	}
	if st := c.session.DoJSON(t, http.MethodDelete, "/auth/keys/"+c.keyID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: revoke key %s got HTTP %d", c.keyID, st)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
