package extidentities

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestVaultRoundTrip(t *testing.T) {
	vault, err := NewVault(CryptoConfig{Key: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_supersecrettoken"

	sealed, err := vault.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, secret) {
		t.Fatal("sealed value leaks the plaintext")
	}
	if !strings.HasPrefix(sealed, "v1."+vault.KeyFingerprint()+".") {
		t.Errorf("sealed value %q lacks the v1.<fingerprint>. prefix", sealed)
	}
	opened, err := vault.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != secret {
		t.Errorf("opened %q, want %q", opened, secret)
	}

	// Sealing is randomized: the same plaintext must not produce the same
	// ciphertext, or equal credentials would be detectable at rest.
	again, err := vault.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if again == sealed {
		t.Error("two seals of the same plaintext produced identical ciphertext")
	}
}

func TestVaultRejectsBadKeys(t *testing.T) {
	for name, key := range map[string]string{
		"empty":      "",
		"not base64": "!!!!",
		"too short":  base64.StdEncoding.EncodeToString([]byte("16-bytes-exactly")),
	} {
		if _, err := NewVault(CryptoConfig{Key: key}); err == nil {
			t.Errorf("%s key: NewVault succeeded, want error", name)
		}
	}
}

func TestVaultWrongKeyCannotOpen(t *testing.T) {
	a, err := NewVault(CryptoConfig{Key: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewVault(CryptoConfig{Key: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("a foreign key opened the value")
	}
}

func TestVaultRotationOpensOldValues(t *testing.T) {
	oldKey := testKey(t)
	old, err := NewVault(CryptoConfig{Key: oldKey})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := old.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := NewVault(CryptoConfig{Key: testKey(t), KeysOld: []string{oldKey}})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := rotated.Open(sealed)
	if err != nil {
		t.Fatalf("rotated vault could not open an old value: %v", err)
	}
	if opened != "secret" {
		t.Errorf("opened %q, want %q", opened, "secret")
	}
	if rotated.SealedWithPrimary(sealed) {
		t.Error("old value reported as sealed with the primary key")
	}
}

func TestPATProviderRoundTrip(t *testing.T) {
	p := newPATProvider(ProviderRecord{Name: "github", Type: ProviderTypePAT, PAT: &PATConfig{Scheme: "bearer"}})
	ctx := context.Background()

	stored, err := p.StoreIdentity(ctx, "ghp_token", "")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ExpiresAt != nil || stored.Refreshable {
		t.Errorf("PAT stored as expiring/refreshable: %+v", stored)
	}
	cred, err := p.RetrieveIdentity(ctx, stored.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Credentials != "ghp_token" {
		t.Errorf("retrieved %q, want %q", cred.Credentials, "ghp_token")
	}

	// The object form carries a username for basic-auth providers.
	stored, err = p.StoreIdentity(ctx, map[string]any{"token": "api-token", "username": "a@b.c"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cred, _ = p.RetrieveIdentity(ctx, stored.Envelope)
	if cred.Credentials != "api-token" || cred.Username != "a@b.c" {
		t.Errorf("retrieved %+v, want token/username preserved", cred)
	}

	if _, refreshed, err := p.Refresh(ctx, stored.Envelope); err != nil || refreshed {
		t.Errorf("PAT Refresh = (%v, %v), want (false, nil)", refreshed, err)
	}
	if _, err := p.StoreIdentity(ctx, map[string]any{"username": "nobody"}, ""); err == nil {
		t.Error("storing a PAT without a token succeeded")
	}
}

// newTestOAuthProvider builds an OAuth provider without touching a vault-
// sealed secret (the secret itself is irrelevant to envelope handling).
func newTestOAuthProvider(t *testing.T) Provider {
	t.Helper()
	vault, err := NewVault(CryptoConfig{Key: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := vault.Seal("client-secret")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(ProviderRecord{
		Name: "google", Type: ProviderTypeOAuth,
		OAuth: &OAuthConfig{
			AuthorizeURL: "https://example.test/auth", TokenURL: "https://example.test/token",
			ClientID: "client", ClientSecretEnc: sealed, Scopes: []string{"email"},
		},
	}, vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOAuthStoreNormalizesCallbackTuple(t *testing.T) {
	p := newTestOAuthProvider(t)
	ctx := context.Background()

	stored, err := p.StoreIdentity(ctx, map[string]any{
		"access_token":  "at-1",
		"refresh_token": "rt-1",
		"token_type":    "Bearer",
		"expires_in":    float64(3600),
		"scope":         "repo read:org",
		"id_token":      "jwt",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Refreshable {
		t.Error("identity with a refresh token is not marked refreshable")
	}
	if stored.ExpiresAt == nil || time.Until(*stored.ExpiresAt) < 59*time.Minute {
		t.Errorf("expires_in was not translated into an expiry: %+v", stored.ExpiresAt)
	}
	if len(stored.Access) != 2 || stored.Access[0] != "repo" {
		t.Errorf("scope string not split into %v", stored.Access)
	}

	// Retrieval exposes the access token and NOT the refresh token.
	cred, err := p.RetrieveIdentity(ctx, stored.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Credentials != "at-1" {
		t.Errorf("retrieved %q, want the access token", cred.Credentials)
	}
	raw, _ := json.Marshal(cred)
	if strings.Contains(string(raw), "rt-1") {
		t.Error("the retrieved credential leaks the refresh token")
	}
	// Provider extras survive for future consumers.
	if !strings.Contains(stored.Envelope, "id_token") {
		t.Error("provider extras were dropped from the envelope")
	}
}

func TestOAuthStorePreservesRefreshToken(t *testing.T) {
	p := newTestOAuthProvider(t)
	ctx := context.Background()

	first, err := p.StoreIdentity(ctx, map[string]any{"access_token": "at-1", "refresh_token": "rt-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Re-consent without access_type=offline returns no refresh token; the
	// working one must survive rather than being overwritten with nothing.
	second, err := p.StoreIdentity(ctx, map[string]any{"access_token": "at-2"}, first.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Refreshable || !strings.Contains(second.Envelope, "rt-1") {
		t.Errorf("re-store dropped the existing refresh token: %s", second.Envelope)
	}
}

func TestOAuthStoreRequiresAccessToken(t *testing.T) {
	p := newTestOAuthProvider(t)
	if _, err := p.StoreIdentity(context.Background(), map[string]any{"refresh_token": "rt"}, ""); err == nil {
		t.Error("storing an OAuth identity without an access token succeeded")
	}
}

func TestDocIDIsUnambiguous(t *testing.T) {
	// Without a separator, ("ab","c") and ("a","bc") would collide.
	if DocID("ab", "c") == DocID("a", "bc") {
		t.Error("doc ids collide across the pair boundary")
	}
	first, second := DocID("u", "p"), DocID("u", strings.Clone("p"))
	if first != second {
		t.Error("doc id is not deterministic")
	}
}

func TestHasAccess(t *testing.T) {
	if !HasAccess([]string{"repo", "user"}, []string{"repo"}) {
		t.Error("granted scope reported as missing")
	}
	if HasAccess([]string{"user"}, []string{"repo"}) {
		t.Error("missing scope reported as granted")
	}
	if !HasAccess(nil, nil) {
		t.Error("empty requirement should always be satisfied")
	}
}

func TestOAuthAccessComesFromTheProvider(t *testing.T) {
	p := newTestOAuthProvider(t)
	ctx := context.Background()

	// The provider reported what it granted: that is authoritative, and the
	// flag tells the service not to overwrite it with the request's wish
	// list (the over-claim a declined permission would otherwise cause).
	granted, err := p.StoreIdentity(ctx, map[string]any{"access_token": "at", "scope": "read"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !granted.AccessFromProvider {
		t.Error("provider-reported scope is not marked as coming from the provider")
	}
	if len(granted.Access) != 1 || granted.Access[0] != "read" {
		t.Errorf("recorded access %v, want [read]", granted.Access)
	}

	// A silent provider leaves only the configured defaults — a guess, and
	// flagged as such so a caller's requested scopes may replace it.
	silent, err := p.StoreIdentity(ctx, map[string]any{"access_token": "at"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if silent.AccessFromProvider {
		t.Error("assumed default scopes are marked as provider-reported")
	}
	if len(silent.Access) != 1 || silent.Access[0] != "email" {
		t.Errorf("fallback access %v, want the provider defaults [email]", silent.Access)
	}
}

func TestResolveAccessExpandsLevels(t *testing.T) {
	rec := ProviderRecord{
		Name: "github",
		AccessLevels: map[string][]string{
			"read":  {"repo:status", "read:org"},
			"admin": {"repo", "admin:org"},
		},
	}

	// A single level resolves to its scopes and is recorded as the level.
	scopes, level := ResolveAccess(rec, []string{"read"})
	if level != "read" || len(scopes) != 2 || !HasAccess(scopes, []string{"repo:status", "read:org"}) {
		t.Errorf("ResolveAccess(read) = (%v, %q), want the level's two scopes", scopes, level)
	}

	// Raw scopes pass through untouched, and claim no level.
	scopes, level = ResolveAccess(rec, []string{"gist"})
	if level != "" || len(scopes) != 1 || scopes[0] != "gist" {
		t.Errorf("ResolveAccess(gist) = (%v, %q), want ([gist], \"\")", scopes, level)
	}

	// Mixing a level with raw scopes yields the union but no level label:
	// the credential is no longer exactly that tier.
	scopes, level = ResolveAccess(rec, []string{"read", "gist"})
	if level != "" || !HasAccess(scopes, []string{"repo:status", "read:org", "gist"}) {
		t.Errorf("ResolveAccess(read+gist) = (%v, %q), want the union with no level", scopes, level)
	}

	// Two levels also produce no single label, and de-duplicate.
	scopes, level = ResolveAccess(rec, []string{"read", "admin"})
	if level != "" || len(scopes) != 4 {
		t.Errorf("ResolveAccess(read+admin) = (%v, %q), want 4 scopes and no level", scopes, level)
	}

	if scopes, level := ResolveAccess(rec, nil); scopes != nil || level != "" {
		t.Errorf("ResolveAccess(nil) = (%v, %q), want (nil, \"\")", scopes, level)
	}
}

func TestValidateAccessLevels(t *testing.T) {
	if err := ValidateAccessLevels(map[string][]string{"read": {"a"}, "admin-2": {"b", "c"}}); err != nil {
		t.Errorf("valid levels rejected: %v", err)
	}
	for name, levels := range map[string]map[string][]string{
		"empty scope list": {"read": {}},
		"blank scope":      {"read": {" "}},
		"uppercase name":   {"Read": {"a"}},
		"name with space":  {"read only": {"a"}},
	} {
		if err := ValidateAccessLevels(levels); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
	if err := ValidateAccessLevels(nil); err != nil {
		t.Errorf("no levels at all should be valid: %v", err)
	}
}
