package crawler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const secretToken = "Bearer super-secret-do-not-leak"

// recordingServer records the Authorization header every request arrived with,
// per path, so a test can assert not merely what was sent but where.
func recordingServer(t *testing.T, seen map[string]string, redirect string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization") + "|" + r.Header.Get("X-Custom-Token")
		if redirect != "" && r.URL.Path == "/start" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>a page with enough words to be extracted at all</p></body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialedFetcher(t *testing.T, rules []CredentialRule) *Fetcher {
	t.Helper()
	return NewFetcher(FetchConfig{
		AllowPrivateAddresses: true, HostDelay: 0, Timeout: 5 * time.Second,
	}).Session(rules)
}

// TestCredentialsAreSentToMatchingURLs is the feature working.
func TestCredentialsAreSentToMatchingURLs(t *testing.T) {
	seen := map[string]string{}
	srv := recordingServer(t, seen, "")
	f := credentialedFetcher(t, []CredentialRule{{
		URLPattern: srv.URL + "/*",
		Headers:    map[string]string{"Authorization": secretToken},
	}})
	if _, err := f.Get(context.Background(), srv.URL+"/page"); err != nil {
		t.Fatal(err)
	}
	if got := seen["/page"]; !strings.Contains(got, secretToken) {
		t.Errorf("the header was not sent; server saw %q", got)
	}
}

// TestCredentialsAreNotSentElsewhere is the first half of not leaking: a URL
// the pattern does not cover gets nothing, even on the same host.
func TestCredentialsAreNotSentElsewhere(t *testing.T) {
	seen := map[string]string{}
	srv := recordingServer(t, seen, "")
	f := credentialedFetcher(t, []CredentialRule{{
		URLPattern: srv.URL + "/private/*",
		Headers:    map[string]string{"Authorization": secretToken},
	}})
	if _, err := f.Get(context.Background(), srv.URL+"/public"); err != nil {
		t.Fatal(err)
	}
	if got := seen["/public"]; strings.Contains(got, "super-secret") {
		t.Errorf("a credential reached a URL its pattern does not cover: %q", got)
	}
}

// TestCredentialsDoNotFollowARedirect is the half that actually bites.
//
// http.Client copies the original request's headers onto every redirect hop.
// It strips Authorization only when the hop crosses to a different DOMAIN, and
// strips a custom header like X-Custom-Token never. A crawl that authenticated
// to an internal wiki and followed a redirect elsewhere would hand its token
// over, and nothing in the crawl's own code would have done anything wrong.
func TestCredentialsDoNotFollowARedirect(t *testing.T) {
	elsewhere := map[string]string{}
	other := recordingServer(t, elsewhere, "")

	seen := map[string]string{}
	srv := recordingServer(t, seen, other.URL+"/landed")

	f := credentialedFetcher(t, []CredentialRule{{
		URLPattern: srv.URL + "/*",
		Headers: map[string]string{
			"Authorization":  secretToken,
			"X-Custom-Token": "custom-secret",
		},
	}})
	if _, err := f.Get(context.Background(), srv.URL+"/start"); err != nil {
		t.Fatal(err)
	}
	if got := seen["/start"]; !strings.Contains(got, secretToken) {
		t.Fatalf("the first hop should have been authenticated; saw %q", got)
	}
	got := elsewhere["/landed"]
	if strings.Contains(got, "super-secret") {
		t.Errorf("Authorization survived a redirect to another host: %q", got)
	}
	if strings.Contains(got, "custom-secret") {
		t.Errorf("a custom credential header survived a redirect to another host: %q — "+
			"http.Client does not strip these, so the transport must", got)
	}
}

// TestCredentialsFromOneRuleDoNotLeakIntoAnother covers the subtler case: two
// sites, two tokens, and a header placed by the first rule must not survive
// into a request matching the second.
func TestCredentialsFromOneRuleDoNotLeakIntoAnother(t *testing.T) {
	seenA := map[string]string{}
	a := recordingServer(t, seenA, "")
	seenB := map[string]string{}
	b := recordingServer(t, seenB, "")

	f := credentialedFetcher(t, []CredentialRule{
		{URLPattern: a.URL + "/*", Headers: map[string]string{"Authorization": "Bearer token-for-a"}},
		{URLPattern: b.URL + "/*", Headers: map[string]string{"X-Custom-Token": "token-for-b"}},
	})
	ctx := context.Background()
	if _, err := f.Get(ctx, a.URL+"/one"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, b.URL+"/two"); err != nil {
		t.Fatal(err)
	}
	if got := seenB["/two"]; strings.Contains(got, "token-for-a") {
		t.Errorf("site B saw site A's credential: %q", got)
	}
	if got := seenB["/two"]; !strings.Contains(got, "token-for-b") {
		t.Errorf("site B did not receive its own credential: %q", got)
	}
}

// TestSessionSharesTheRateLimiter keeps politeness a property of the site
// rather than of how many crawls happen to hold credentials for it.
func TestSessionSharesTheRateLimiter(t *testing.T) {
	base := NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0, Timeout: time.Second})
	session := base.Session([]CredentialRule{{
		URLPattern: "https://example.com/*", Headers: map[string]string{"A": "b"}},
	})
	if session.slots == nil || &session.lastSeen == nil {
		t.Fatal("session lost its limiter state")
	}
	// Same underlying map and mutex, so the delay is enforced across both.
	session.lastSeen["example.com"] = time.Now()
	if _, ok := base.lastSeen["example.com"]; !ok {
		t.Error("the credentialed session does not share the host delay with its parent")
	}
	if base.Session(nil) != base {
		t.Error("Session(nil) should return the fetcher itself, not a copy")
	}
}

// TestCredentialHostsNeverIncludeValues guards what may be logged.
func TestCredentialHostsNeverIncludeValues(t *testing.T) {
	hosts := CredentialHosts([]CredentialRule{
		{URLPattern: "https://jira.example.com/browse/*",
			Headers: map[string]string{"Authorization": secretToken}},
	})
	joined := strings.Join(hosts, " ")
	if strings.Contains(joined, "secret") {
		t.Errorf("CredentialHosts leaked a value: %q", joined)
	}
	if joined != "jira.example.com" {
		t.Errorf("CredentialHosts() = %q, want the host", joined)
	}
}

var _ = url.Parse
