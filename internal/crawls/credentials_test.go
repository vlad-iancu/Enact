package crawls

import (
	"strings"
	"testing"
)

// fakeVault seals reversibly without key material, so these tests exercise the
// sealing LOGIC rather than the cipher (which internal/secrets covers).
type fakeVault struct{ opens int }

func (f *fakeVault) Seal(plaintext string) (string, error) {
	return "v1.deadbeef." + plaintext, nil
}

func (f *fakeVault) Open(sealed string) (string, error) {
	f.opens++
	return strings.TrimPrefix(sealed, "v1.deadbeef."), nil
}

func withCreds(rules []CredentialRule) Crawl {
	c := valid()
	c.Credentials = rules
	return c
}

// TestCredentialPatternMustNameAHost is the rule that stops a secret from
// travelling. A pattern matching everything would present a token to whatever
// the crawl wandered onto, and a crawl wanders onto whatever somebody linked.
func TestCredentialPatternMustNameAHost(t *testing.T) {
	header := map[string]string{"Authorization": "Bearer x"}
	refused := []string{
		"*",
		"https://*",
		"https://*/browse/*",
		"*/browse/*",
		"jira.example.com/*", // no scheme: ambiguous about what the host even is
		"https://*.com/*",    // wildcards a whole public suffix
	}
	for _, pattern := range refused {
		msg, ok := Validate(withCreds([]CredentialRule{{URLPattern: pattern, Headers: header}}))
		if ok {
			t.Errorf("pattern %q was accepted; a credential must not go to every site", pattern)
			continue
		}
		if !strings.Contains(msg, "url_pattern") && !strings.Contains(msg, "host") {
			t.Errorf("pattern %q rejected with an unhelpful message: %s", pattern, msg)
		}
	}
	accepted := []string{
		"https://jira.example.com/*",
		"https://jira.example.com/browse/*",
		"https://*.example.com/",
		"http://internal.example.com/*",
	}
	for _, pattern := range accepted {
		if msg, ok := Validate(withCreds([]CredentialRule{{URLPattern: pattern, Headers: header}})); !ok {
			t.Errorf("pattern %q was refused: %s", pattern, msg)
		}
	}
}

// TestForbiddenHeadersAreRefused covers the two reasons a header is off
// limits: it is the transport's to compute, or it is a promise the platform
// makes to the sites it crawls.
func TestForbiddenHeadersAreRefused(t *testing.T) {
	for _, name := range []string{"Host", "Content-Length", "User-Agent", "Connection", "host", "USER-AGENT"} {
		msg, ok := Validate(withCreds([]CredentialRule{{
			URLPattern: "https://x.example.com/*",
			Headers:    map[string]string{name: "value"},
		}}))
		if ok {
			t.Errorf("header %q was accepted", name)
			continue
		}
		if !strings.Contains(msg, "may not be set") {
			t.Errorf("header %q rejected with: %s", name, msg)
		}
	}
}

// TestHeaderValuesCannotInjectASecondHeader.
func TestHeaderValuesCannotInjectASecondHeader(t *testing.T) {
	for _, value := range []string{"a\r\nX-Injected: yes", "a\nb", "a\x00b"} {
		if _, ok := Validate(withCreds([]CredentialRule{{
			URLPattern: "https://x.example.com/*",
			Headers:    map[string]string{"Authorization": value},
		}})); ok {
			t.Errorf("value %q was accepted; it can forge a second header", value)
		}
	}
	if _, ok := Validate(withCreds([]CredentialRule{{
		URLPattern: "https://x.example.com/*",
		Headers:    map[string]string{"X-Bad Name": "v"},
	}})); ok {
		t.Error("a header name with a space was accepted")
	}
}

// TestSealOpenRoundTrip, and that sealing twice does not double-encrypt — which
// is what lets an update that does not resend the headers leave them alone.
func TestSealOpenRoundTrip(t *testing.T) {
	vault := &fakeVault{}
	rules := []CredentialRule{{
		URLPattern: "https://x.example.com/*",
		Headers:    map[string]string{"Authorization": "Bearer secret"},
	}}
	if err := SealCredentials(vault, rules); err != nil {
		t.Fatal(err)
	}
	if rules[0].Headers["Authorization"] == "Bearer secret" {
		t.Fatal("the value was stored in the clear")
	}
	sealed := rules[0].Headers["Authorization"]

	if err := SealCredentials(vault, rules); err != nil {
		t.Fatal(err)
	}
	if rules[0].Headers["Authorization"] != sealed {
		t.Error("sealing an already-sealed value changed it; an update would double-encrypt")
	}

	opened, err := OpenCredentials(vault, rules)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened[0].Headers["Authorization"]; got != "Bearer secret" {
		t.Errorf("opened to %q", got)
	}
	// Opening must not disturb the stored copy.
	if rules[0].Headers["Authorization"] != sealed {
		t.Error("OpenCredentials mutated the sealed rules")
	}
}

// TestRedactedKeepsNamesAndLosesValues is the API contract: an owner can see
// what a crawl is configured to send and where, and can never read it back.
func TestRedactedKeepsNamesAndLosesValues(t *testing.T) {
	c := valid()
	c.Credentials = []CredentialRule{{
		URLPattern: "https://jira.example.com/*",
		Headers:    map[string]string{"Authorization": "Bearer super-secret"},
	}}
	got := c.Redacted()
	rule := got.Credentials[0]
	if rule.URLPattern != "https://jira.example.com/*" {
		t.Error("the pattern was lost; an owner must be able to see where a crawl authenticates")
	}
	if _, ok := rule.Headers["Authorization"]; !ok {
		t.Error("the header name was lost; an owner must be able to see what is sent")
	}
	if rule.Headers["Authorization"] != "" {
		t.Errorf("the value survived redaction: %q", rule.Headers["Authorization"])
	}
	// The original must be untouched, or redacting for a response would
	// destroy what we are about to store.
	if c.Credentials[0].Headers["Authorization"] != "Bearer super-secret" {
		t.Error("Redacted mutated the crawl it was called on")
	}
}

// TestJIRADepthIsBounded. The crawl's own depth ceiling is not enough here: a
// web link is one step to one page, an issue relationship is one step to every
// subtask, every linked issue and the parent, reciprocally.
func TestJIRADepthIsBounded(t *testing.T) {
	base := func(depth int) Crawl {
		c := valid()
		c.Source = SourceJIRA
		c.SeedURLs = []string{"SCRUM-1"}
		c.JIRA = &JIRAConfig{
			BaseURL: "https://acme.atlassian.net", Email: "a@b.c",
			Token: "t", MaxDepth: depth,
		}
		return c
	}
	for _, depth := range []int{0, 1, JIRAMaxDepthCeiling} {
		if msg, ok := Validate(base(depth)); !ok {
			t.Errorf("jira.max_depth %d was refused: %s", depth, msg)
		}
	}
	for _, depth := range []int{-1, JIRAMaxDepthCeiling + 1, 100} {
		msg, ok := Validate(base(depth))
		if ok {
			t.Errorf("jira.max_depth %d was accepted", depth)
			continue
		}
		if !strings.Contains(msg, "jira.max_depth") {
			t.Errorf("depth %d refused with an unhelpful message: %s", depth, msg)
		}
	}
}
