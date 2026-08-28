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
	"path/filepath"
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

// What the inference API accepts as context files, which is what the Converse
// API accepts: a handful of documents, each a few megabytes, in the formats
// Bedrock can read.
//
// enact-model-inference enforces these; they are stated here so a caller can
// refuse a file before base64-encoding megabytes of it into a request that
// would come back a 400. If the service's limits move, these move with them.
const (
	MaxContextFiles     = 5
	MaxContextFileBytes = 4_500_000
)

// contextFileExtensions are the filename extensions the inference API maps
// onto a Converse document format. The format is derived from the NAME, so a
// file whose name lacks a usable extension cannot be attached however well
// formed its bytes are.
var contextFileExtensions = map[string]bool{
	".pdf": true, ".csv": true, ".doc": true, ".docx": true,
	".xls": true, ".xlsx": true, ".html": true, ".htm": true,
	".txt": true, ".md": true,
}

// SupportsContextFile reports whether filename names a document the inference
// API can attach.
func SupportsContextFile(filename string) bool {
	return contextFileExtensions[strings.ToLower(filepath.Ext(filename))]
}

// ContextFileFormats lists the accepted extensions, for an error message that
// tells the author what would have worked.
func ContextFileFormats() string {
	return "pdf, csv, doc, docx, xls, xlsx, html, htm, txt, md"
}

// Request mirrors the inference API's request body. Exactly one of AgentID
// or Model must be set (the callee validates).
type Request struct {
	AgentID  string    `json:"agent_id,omitempty"`
	Model    string    `json:"model,omitempty"`
	Messages []Message `json:"messages"`
	// Temperature and TopP are the model's sampling parameters, both 0–1 and
	// both pointers so "unset" reaches the model as genuinely unset — sending
	// a zero would mean fully deterministic, which is a choice, not a default.
	// They apply to agent invocations too: naming an agent fixes its model,
	// prompt and output schema, not how its replies are sampled.
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        *float32 `json:"top_p,omitempty"`
	// RetrievalTopK is not a sampling parameter — it is how many passages the
	// agent's retrieval knowledge base contributes. Only meaningful with
	// AgentID.
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

// Response is a buffered inference result.
type Response struct {
	Content      string `json:"content"`
	StopReason   string `json:"stop_reason"`
	InputTokens  int32  `json:"input_tokens"`
	OutputTokens int32  `json:"output_tokens"`
	Model        string `json:"model"`
}

// Invoke runs a NON-streaming inference call and returns the whole reply.
//
// For callers with no one to stream to — a workflow step's output is only
// useful once it is complete, and its consumer is the next step rather than a
// person watching text appear. Streaming and then reassembling would add a
// parser between the model and the record for no benefit.
//
// No client-level timeout applies (see NewClient); the context governs, which
// matters because an agent with tools can legitimately take minutes.
func (c *Client) Invoke(ctx context.Context, req Request) (Response, error) {
	req.Stream = false
	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("inference: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/inference", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("inference: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("inference: call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		// The message is passed through rather than wrapped in a status code,
		// because it is usually the inference service explaining something the
		// user can act on — an agent they may not use, or a schema the model
		// could not satisfy.
		return Response{}, fmt.Errorf("%s", apiErr.Error)
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("inference: decode response: %w", err)
	}
	return out, nil
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
