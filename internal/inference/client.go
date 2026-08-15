// Package inference holds the client wrapper other services use to invoke
// the enact-model-inference service — per the repository layout rules,
// clients live in domain packages, never in another service's internals.
package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling enact-model-inference.
type ClientConfig struct {
	// BaseURL is the root URL of the inference service, without a trailing
	// path. The default matches the port the README assigns it.
	BaseURL string `env:"INFERENCE_API_URL, default=http://localhost:8080"`
}

// Message is one chat turn sent to the inference API.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls are the MCP tools an assistant message used. Replayed into
	// the model's context so a later turn can see what was already done and
	// what it returned, rather than only the answer that came out of it.
	ToolCalls []MessageToolCall `json:"tool_calls,omitempty"`
}

// MessageToolCall is one recorded tool invocation being replayed. Arguments
// are the model's own, as it produced them.
type MessageToolCall struct {
	ServerID          string          `json:"server_id"`
	Tool              string          `json:"tool"`
	ToolUseID         string          `json:"tool_use_id"`
	Arguments         json.RawMessage `json:"arguments,omitempty"`
	Content           string          `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	IsError           bool            `json:"is_error,omitempty"`
	// Turn is the tool-loop round; Text is the assistant's words in that
	// round, before the call.
	Turn int    `json:"turn,omitempty"`
	Text string `json:"text,omitempty"`
}

// ContextFile is one ad-hoc document attached to a request; Content is the
// file's raw bytes base64-encoded. The callee passes it to the model
// natively as a Bedrock DocumentBlock.
type ContextFile struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// Request mirrors the inference API's request body. Exactly one of AgentID
// or Model must be set (the callee validates).
type Request struct {
	AgentID       string        `json:"agent_id,omitempty"`
	Model         string        `json:"model,omitempty"`
	Messages      []Message     `json:"messages"`
	RetrievalTopK *int          `json:"retrieval_top_k,omitempty"`
	ContextFiles  []ContextFile `json:"context_files,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

// StreamEvent is one SSE event from a streaming inference call. Event is
// "message" for plain data chunks (the default SSE event type), or the
// explicit name ("meta", "error") for named events. Data is the raw JSON
// payload.
type StreamEvent struct {
	Event string
	Data  string
}

// Client calls the inference service over HTTP.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the inference service at cfg.BaseURL. base
// is the innermost RoundTripper — in practice the caller's S2S signing
// transport — wrapped by the tracing transport; nil means plain
// http.DefaultTransport.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	return &Client{
		// Deliberately no Client.Timeout: it would cover the whole response
		// body and sever long generations mid-stream. The request context
		// (cancelled when the browser disconnects) governs the stream's life.
		http:    &http.Client{Transport: requesthelper.NewTransport(base)},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// Stream runs a streaming inference call, invoking onEvent for every SSE
// event as it arrives. The calling user is taken from ctx and forwarded as
// the platform's stub-auth header. A non-nil error from onEvent aborts the
// stream and is returned.
func (c *Client) Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) error {
	req.Stream = true
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("inference: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/inference", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("inference: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("inference: stream call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A non-200 means the request was rejected before streaming began; the
	// body is a regular JSON error.
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("inference: %s", apiErr.Error)
	}

	// Minimal SSE parse: accumulate event/data field lines, dispatch on the
	// blank line that terminates each event.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	event := StreamEvent{Event: "message"}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if event.Data != "" {
				if err := onEvent(event); err != nil {
					return err
				}
			}
			event = StreamEvent{Event: "message"}
		case strings.HasPrefix(line, "event: "):
			event.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.Data += strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("inference: read stream: %w", err)
	}
	return nil
}
