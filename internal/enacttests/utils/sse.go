package utils

import (
	"bufio"
	"io"
	"net/http"
	"strings"
)

// StreamSSE posts to path within the session and invokes onEvent for each
// SSE event until it returns false, the stream ends, or the case's context
// is cancelled.
//
// It uses a client with NO timeout, unlike DoJSON: a stream can legitimately
// stay open for minutes (a tool call waiting for the user to connect an
// account), and the session client's timeout would sever it mid-flight.
// The case's context is what bounds it.
func (s *MainSession) StreamSSE(t *T, path string, body io.Reader, onEvent func(event, data string) bool) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.baseURL+path, body)
	if err != nil {
		t.Fatalf("build stream request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Jar: s.client.Jar}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: got HTTP %d, want 200", path, resp.StatusCode)
	}

	// Same minimal parse as internal/inference/client.go: accumulate the
	// event/data lines, dispatch on the blank line that ends each event.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	event, data := "message", ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data != "" && !onEvent(event, data) {
				return
			}
			event, data = "message", ""
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
}
