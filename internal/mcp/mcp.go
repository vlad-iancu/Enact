// Package mcp wraps the official MCP go-sdk with the small client surface
// the platform needs: connect to a server over either supported transport,
// list its tools, and call one. Sessions are short-lived — connect, use,
// close — because callers (the registry's cache refresh, the inference
// tool loop) have no long-running conversation with the servers.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport names, matching the platform's MCP server records.
const (
	TransportStreamableHTTP = "streamable-http"
	TransportSSE            = "sse"
)

// ValidTransport reports whether t names a supported MCP transport.
func ValidTransport(t string) bool {
	return t == TransportStreamableHTTP || t == TransportSSE
}

// Tool is the platform's normalized view of an MCP tool definition. Schema
// and metadata fields stay raw JSON: the platform stores and forwards them
// without interpreting their contents.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Meta         json.RawMessage `json:"meta,omitempty"`
}

// Session is an initialized connection to one MCP server.
type Session struct {
	cs *sdk.ClientSession
}

// Connect dials and initializes an MCP session against url using the named
// transport ("streamable-http" or "sse"). Pass nil for the default HTTP
// client. The caller must Close the session.
func Connect(ctx context.Context, url, transport string, hc *http.Client) (*Session, error) {
	var t sdk.Transport
	switch transport {
	case TransportStreamableHTTP:
		t = &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: hc}
	case TransportSSE:
		t = &sdk.SSEClientTransport{Endpoint: url, HTTPClient: hc}
	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q", transport)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "enact-platform", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect %s (%s): %w", url, transport, err)
	}
	return &Session{cs: cs}, nil
}

// Close terminates the session.
func (s *Session) Close() error { return s.cs.Close() }

// ListTools returns every tool the server advertises, following pagination.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	var cursor string
	for {
		res, err := s.cs.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		for _, t := range res.Tools {
			out = append(out, normalizeTool(t))
		}
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// CallResult is the outcome of one tool call, flattened for the platform:
// text content joined, structured content kept raw.
type CallResult struct {
	// Content is the concatenated text of the result's content blocks.
	Content string `json:"content"`
	// StructuredContent is the structured result verbatim, when present.
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	// IsError reports a tool-level failure (the call itself succeeded).
	IsError bool `json:"is_error,omitempty"`
}

// CallTool invokes one tool with the given JSON arguments.
func (s *Session) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	var args any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return CallResult{}, fmt.Errorf("mcp: tool %q arguments are not valid JSON: %w", name, err)
		}
	}
	res, err := s.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return CallResult{}, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}
	out := CallResult{IsError: res.IsError}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	out.Content = strings.Join(texts, "\n")
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			out.StructuredContent = raw
		}
	}
	return out, nil
}

// normalizeTool converts an SDK tool to the platform shape, marshaling the
// loosely-typed schema/metadata fields to raw JSON.
func normalizeTool(t *sdk.Tool) Tool {
	out := Tool{Name: t.Name, Title: t.Title, Description: t.Description}
	out.InputSchema = marshalRaw(t.InputSchema)
	out.OutputSchema = marshalRaw(t.OutputSchema)
	if t.Annotations != nil {
		out.Annotations = marshalRaw(t.Annotations)
	}
	if len(t.Meta) > 0 {
		out.Meta = marshalRaw(t.Meta)
	}
	return out
}

func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
