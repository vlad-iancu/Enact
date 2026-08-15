package utils

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// IdentitiesAudience is the external identity service's S2S key id.
const IdentitiesAudience = "enact-external-identities"

// IdentitiesURL builds a URL against the external identity service.
func (t *T) IdentitiesURL(path string) string { return t.Env.IdentitiesURL + path }

// DoPlainJSON performs an unauthenticated request and decodes a JSON body.
// For third parties rather than platform services — the OAuth fixture's
// introspection, which no service token applies to.
func (t *T) DoPlainJSON(method, url string, out any) int {
	req, err := http.NewRequestWithContext(t.ctx, method, url, nil)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	resp, err := (&http.Client{Timeout: t.Env.Timeout}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

// DoPlain performs an unauthenticated request that FOLLOWS redirects, the
// way a browser would. The OAuth cases need it: the consent URL points at a
// third party (the fixture provider), which redirects back to the platform's
// callback — a hop no service token applies to.
func (t *T) DoPlain(method, url string) int {
	req, err := http.NewRequestWithContext(t.ctx, method, url, nil)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	resp, err := (&http.Client{Timeout: t.Env.Timeout}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode
}

// DoJSONRaw is DoJSON plus the raw response body, for cases that must assert
// on what a payload does NOT contain (e.g. that a listing never carries a
// stored credential). out may be nil.
func (t *T) DoJSONRaw(as, audience, method, url string, body io.Reader, out any) (int, string) {
	client, err := t.Env.Client(as, audience)
	if err != nil {
		t.Fatalf("build client for %q: %v", as, err)
	}
	req, err := http.NewRequestWithContext(t.ctx, method, url, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", t.Env.UserID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read response: %v", method, url, err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.NewDecoder(strings.NewReader(string(raw))).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, url, err)
		}
	}
	return resp.StatusCode, string(raw)
}
