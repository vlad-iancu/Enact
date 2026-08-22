package enactmain

import (
	"net/http"
	"net/http/httptest"
	"testing"

	restful "github.com/emicklei/go-restful/v3"
)

func request(headers map[string]string) *restful.Request {
	r := httptest.NewRequest(http.MethodGet, "/agents", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return restful.NewRequest(r)
}

// TestConsumeAPIKeyClearsAuthorization is the guard on a cross-package
// assumption: enact-main registers the S2S filter on every WebService, and it
// reads Authorization. A user's API key is not a service token, so it must
// not still be in that header when the S2S filter looks.
//
// Without this, a valid key produces "invalid service token: token is
// malformed" on any deployment with S2S enabled — which is the default — and
// nothing in a local run (where S2S is off) would show it.
func TestConsumeAPIKeyClearsAuthorization(t *testing.T) {
	req := request(map[string]string{"Authorization": "Bearer enact_sk_abc123"})

	if got := consumeAPIKey(req); got != "enact_sk_abc123" {
		t.Fatalf("consumeAPIKey = %q, want the key", got)
	}
	if left := req.Request.Header.Get("Authorization"); left != "" {
		t.Errorf("Authorization survived as %q; the S2S filter will try to parse it as a service token", left)
	}
}

func TestConsumeAPIKeyDedicatedHeader(t *testing.T) {
	req := request(map[string]string{apiKeyHeader: "enact_sk_abc123"})

	if got := consumeAPIKey(req); got != "enact_sk_abc123" {
		t.Fatalf("consumeAPIKey = %q, want the key", got)
	}
	if left := req.Request.Header.Get(apiKeyHeader); left != "" {
		t.Errorf("%s survived as %q; a credential should not outlive its use", apiKeyHeader, left)
	}
}

// A request carrying a real service token AND a key in the dedicated header
// must keep the token: it is not ours to consume, and removing it would make
// the caller anonymous to the S2S layer.
func TestConsumeAPIKeyLeavesServiceTokens(t *testing.T) {
	const serviceToken = "Bearer eyJhbGciOiJFZERTQSIsImtpZCI6ImVuYWN0LXRlc3RzIn0.e30.sig"
	req := request(map[string]string{
		"Authorization": serviceToken,
		apiKeyHeader:    "enact_sk_abc123",
	})

	if got := consumeAPIKey(req); got != "enact_sk_abc123" {
		t.Fatalf("consumeAPIKey = %q, want the dedicated header to win", got)
	}
	if left := req.Request.Header.Get("Authorization"); left != serviceToken {
		t.Errorf("Authorization = %q, want the service token untouched", left)
	}
}

func TestConsumeAPIKeyIgnoresOtherSchemes(t *testing.T) {
	for _, header := range []string{"", "Basic dXNlcjpwYXNz", "Bearer", "Token enact_sk_abc"} {
		req := request(map[string]string{"Authorization": header})
		if got := consumeAPIKey(req); got != "" {
			t.Errorf("Authorization %q yielded key %q, want none", header, got)
		}
		// Nothing was consumed, so nothing may be removed — Basic auth and
		// service tokens both belong to somebody else.
		if header != "" && req.Request.Header.Get("Authorization") != header {
			t.Errorf("Authorization %q was cleared despite not being an API key", header)
		}
	}
}

// A bearer value without the enact_sk_ prefix is somebody else's credential —
// in practice an S2S token. It must pass through untouched so the S2S filter
// can verify it, which matters most on the auth group, where optionalCaller
// now runs on routes services are permitted to call.
func TestConsumeAPIKeyLeavesUnprefixedBearerAlone(t *testing.T) {
	const serviceToken = "Bearer eyJhbGciOiJFZERTQSIsImtpZCI6ImVuYWN0LXRlc3RzIn0.e30.sig"
	req := request(map[string]string{"Authorization": serviceToken})

	if got := consumeAPIKey(req); got != "" {
		t.Errorf("a service token was claimed as an API key: %q", got)
	}
	if left := req.Request.Header.Get("Authorization"); left != serviceToken {
		t.Errorf("Authorization = %q, want the service token untouched", left)
	}
}

// hashAPIKey is what a stored key is matched by; it must be stable, or every
// key in the index stops working at once.
func TestHashAPIKeyIsStableSHA256(t *testing.T) {
	// sha256("enact_sk_abc123"), computed outside this codebase:
	//   printf 'enact_sk_abc123' | shasum -a 256
	const want = "26e945216b412f97340a999423bb73a7f18c45ade503020df57ba50a300ee7c3"
	if got := hashAPIKey("enact_sk_abc123"); len(got) != 64 {
		t.Fatalf("hashAPIKey returned %d chars, want 64 hex", len(got))
	} else if got != want {
		t.Errorf("hashAPIKey = %s, want %s — changing this invalidates every stored key", got, want)
	}
}

func TestGenerateAPIKeyShape(t *testing.T) {
	plaintext, key, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey: %v", err)
	}
	if len(plaintext) != len(keyPrefix)+keyRandomChars {
		t.Errorf("key length %d, want %d", len(plaintext), len(keyPrefix)+keyRandomChars)
	}
	if key.KeyHash != hashAPIKey(plaintext) {
		t.Errorf("stored hash does not match the issued key")
	}
	if key.Prefix != plaintext[:displayPrefixLen] {
		t.Errorf("prefix %q is not the head of the key", key.Prefix)
	}
	// The record must never carry the key itself.
	if key.Prefix == plaintext {
		t.Errorf("the stored prefix IS the whole key")
	}

	second, _, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey (second): %v", err)
	}
	if second == plaintext {
		t.Errorf("two generated keys are identical")
	}
}
