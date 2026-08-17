// Package utils is the toolkit test cases are written with: the per-case
// handle T (assertions + HTTP helpers), the runtime Env (target URLs and
// impersonation), the TestCase lifecycle contract, and the domain helpers
// for agents, knowledge bases, and inference.
package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"enact/internal/logging"
)

// T is the per-case handle passed to every lifecycle phase: assertions,
// logging, and pre-authenticated HTTP helpers. Fatalf aborts the phase via
// panic, which RunPhase recovers — the same mechanism testing.T uses via
// runtime.Goexit.
type T struct {
	Name string
	Env  *Env

	ctx     context.Context
	logger  *logging.Logger
	failed  bool
	failure string
}

// NewT builds the handle for one case run.
func NewT(name string, env *Env, ctx context.Context, logger *logging.Logger) *T {
	return &T{Name: name, Env: env, ctx: ctx, logger: logger}
}

// Failed reports whether any assertion failed so far.
func (t *T) Failed() bool { return t.failed }

// Failure returns the first failure message, or "".
func (t *T) Failure() string { return t.failure }

// fatalSentinel is the panic payload Fatalf uses to abort a phase.
type fatalSentinel struct{}

// Context returns the execution-scoped context for outbound requests.
func (t *T) Context() context.Context { return t.ctx }

// Logf records progress; it lands in the service log tagged with the case.
func (t *T) Logf(format string, args ...any) {
	t.logger.Info("test log", "case", t.Name, "msg", fmt.Sprintf(format, args...))
}

// Errorf marks the case failed and continues.
func (t *T) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	t.failed = true
	if t.failure == "" {
		t.failure = msg
	}
	t.logger.Warn("test assertion failed", "case", t.Name, "msg", msg)
}

// Fatalf marks the case failed and aborts the current phase immediately.
func (t *T) Fatalf(format string, args ...any) {
	t.Errorf(format, args...)
	panic(fatalSentinel{})
}

// RunPhase executes one lifecycle phase of a case, translating Fatalf
// aborts (and any other panic) into a recorded failure so a crashing phase
// never kills its worker and never prevents phases that must still run.
func RunPhase(t *T, phase string, fn func(*T)) {
	defer func() {
		if rec := recover(); rec != nil {
			if _, isFatal := rec.(fatalSentinel); !isFatal {
				t.failed = true
				if t.failure == "" {
					t.failure = fmt.Sprintf("%s panic: %v", phase, rec)
				}
			}
		}
	}()
	fn(t)
}

// Eventually retries check every 250ms until it reports success or the
// timeout elapses, then fails the case with the check's last message. It
// exists for assertions over search-backed listings: OpenSearch only
// surfaces writes after an index refresh (~1s), while direct GETs are
// realtime.
func (t *T) Eventually(timeout time.Duration, desc string, check func() (bool, string)) {
	deadline := time.Now().Add(timeout)
	var lastMsg string
	for {
		ok, msg := check()
		if ok {
			return
		}
		lastMsg = msg
		if time.Now().After(deadline) {
			t.Errorf("%s: not satisfied within %s: %s", desc, timeout, lastMsg)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// DoMultipart uploads one file as multipart/form-data (field "file") as the
// given service identity, decodes the JSON response into out (unless nil),
// and returns the status code. A transport-level failure aborts the phase.
func (t *T) DoMultipart(as, audience, url, filename string, content []byte, out any) int {
	client, err := t.Env.Client(as, audience)
	if err != nil {
		t.Fatalf("build client for %q: %v", as, err)
	}
	body, contentType, err := buildMultipart(filename, content)
	if err != nil {
		t.Fatalf("build multipart: %v", err)
	}
	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("build upload request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-User-Id", t.Env.UserID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("POST %s: decode response: %v", url, err)
		}
	}
	return resp.StatusCode
}

// buildMultipart assembles a single-file multipart body under field "file".
func buildMultipart(filename string, content []byte) (io.Reader, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(content); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &buf, writer.FormDataContentType(), nil
}

// DoJSON performs an HTTP request as the given service identity, decodes the
// JSON response body into out (unless nil), and returns the status code. It
// stamps the test user id; a transport-level failure aborts the phase.
func (t *T) DoJSON(as, audience, method, url string, body io.Reader, out any) int {
	return t.DoJSONAs(t.Env.UserID, as, audience, method, url, body, out)
}

// DoJSONAs is DoJSON acting as a specific user rather than the suite's
// default. Needed wherever a case creates something through a browser
// session and then calls a service directly about it: the two must be the
// same person, or authorization correctly refuses.
func (t *T) DoJSONAs(userID, as, audience, method, url string, body io.Reader, out any) int {
	client, err := t.Env.Client(as, audience)
	if err != nil {
		t.Fatalf("build client for %q: %v", as, err)
	}
	req, err := http.NewRequestWithContext(t.ctx, method, url, body)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	// Always JSON, even with an empty body: go-restful 415s a bodyless POST
	// to a Consumes(JSON) route when the header is missing.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", userID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("%s %s: decode response: %v", method, url, err)
		}
	}
	return resp.StatusCode
}
