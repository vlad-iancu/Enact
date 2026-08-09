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
}

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
		return
	case http.StatusConflict:
		login := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
		if st := s.DoJSON(t, http.MethodPost, "/auth/login", strings.NewReader(login), nil); st != http.StatusOK {
			t.Fatalf("login as %s: got HTTP %d, want 200", email, st)
		}
	default:
		t.Fatalf("register %s: got HTTP %d, want 201 or 409", email, status)
	}
}
