package utils

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
)

// MainSession is a browser-like client against the enact-main service: a
// cookie jar carries the session across calls, and — unlike T.DoJSON — no
// service token or X-User-Id header is sent, because enact-main
// authenticates PEOPLE via sessions, not services.
type MainSession struct {
	client  *http.Client
	baseURL string
	// userID is the authenticated account's id, resolved at login. Cases
	// that then call a service DIRECTLY need it: a service call carries
	// whatever user id it is told to, and using the suite's default would
	// act as a different person than the one that created the fixtures.
	userID string
}

// UserID is the authenticated account's id.
func (s *MainSession) UserID() string { return s.userID }

// NewMainSession returns a fresh, unauthenticated session client.
func (t *T) NewMainSession() *MainSession {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	return &MainSession{
		client:  &http.Client{Jar: jar, Timeout: t.Env.Timeout},
		baseURL: strings.TrimRight(t.Env.MainAPIURL, "/"),
	}
}

// DoJSON performs a request within the session, decodes the JSON response
// into out (unless nil), and returns the status code. A transport failure
// aborts the phase.
func (s *MainSession) DoJSON(t *T, method, path string, body io.Reader, out any) int {
	req, err := http.NewRequestWithContext(t.Context(), method, s.baseURL+path, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// DoAPIKey performs a request against enact-main authenticated by an API key
// instead of a session — no cookie jar, exactly as an external caller would.
//
// extraHeaders exists for one purpose: the impersonation check, which sends a
// forged X-User-Id alongside a valid key and asserts the response is still
// scoped to the key's owner.
func (t *T) DoAPIKey(key, method, path string, body io.Reader, out any, extraHeaders map[string]string) int {
	req, err := http.NewRequestWithContext(t.Context(), method,
		strings.TrimRight(t.Env.MainAPIURL, "/")+path, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	// A bare client: no jar, so nothing can accidentally carry a session and
	// make a key look like it worked when it did not.
	resp, err := (&http.Client{Timeout: t.Env.Timeout}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// DoMultipart uploads one file (field "file") within the session.
func (s *MainSession) DoMultipart(t *T, path, filename string, content []byte, out any) int {
	body, contentType, err := buildMultipart(filename, content)
	if err != nil {
		t.Fatalf("build multipart: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.baseURL+path, body)
	if err != nil {
		t.Fatalf("build upload request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("POST %s: decode response: %v", path, err)
		}
	}
	return resp.StatusCode
}

// RegisterOrLogin authenticates the session as the given local account,
// creating it on first use. Test accounts are fixed, so reruns hit the 409
// path and fall back to login.
func (s *MainSession) RegisterOrLogin(t *T, displayName, email, password string) {
	body := fmt.Sprintf(`{"display_name":%q,"email":%q,"password":%q}`, displayName, email, password)
	status := s.DoJSON(t, http.MethodPost, "/auth/register", strings.NewReader(body), nil)
	switch status {
	case http.StatusCreated:
	case http.StatusConflict:
		login := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
		if st := s.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st != http.StatusOK {
			t.Fatalf("login as %s: got HTTP %d, want 200", email, st)
		}
	default:
		t.Fatalf("register %s: got HTTP %d, want 201 or 409", email, status)
	}
	s.provision(t, email)
}

// provision places a freshly authenticated fixture account in the suite's
// organization and gives it the create rules.
//
// A self-registered account belongs to no organization, and since RBAC
// landed that means it can do nothing at all — not create a resource, not
// even connect an identity, because a provider is resolved through the
// caller's organization. Every case that logs in as a person needs this, so
// it happens here rather than being repeated (and forgotten) per case.
//
// Only CREATE rules are granted. The isolation cases depend on a fixture
// account being unable to reach another account's resources, so anything
// broader would quietly stop them proving anything.
func (s *MainSession) provision(t *T, email string) {
	var me struct {
		ID string `json:"id"`
	}
	if st := s.DoJSON(t, http.MethodGet, "/auth/me", nil, &me); st != http.StatusOK || me.ID == "" {
		t.Fatalf("resolve the id of %s: got HTTP %d (id=%q)", email, st, me.ID)
	}
	if err := t.Env.PlaceInOrganization(t.Context(), me.ID); err != nil {
		t.Fatalf("place %s in the suite's organization: %v", email, err)
	}
	if err := t.Env.GrantRules(t.Context(), me.ID, TestRules()); err != nil {
		t.Fatalf("grant create permissions to %s: %v", email, err)
	}
	s.userID = me.ID
}

// DoJSONRaw is DoJSON plus the raw response body, for assertions about what
// a payload must NOT contain (e.g. that a provider response never echoes a
// client secret).
func (s *MainSession) DoJSONRaw(t *T, method, path string, body io.Reader, out any) (int, string) {
	req, err := http.NewRequestWithContext(t.Context(), method, s.baseURL+path, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read response: %v", method, path, err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.NewDecoder(strings.NewReader(string(raw))).Decode(out); err != nil {
			t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode, string(raw)
}
