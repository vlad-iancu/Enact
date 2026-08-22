package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer builds the Google Workspace MCP server for one caller's token.
//
// A server per request rather than one shared instance: the token is the
// caller's, and binding it into the tool handlers is the only way to keep one
// request's credential from being visible to another. Construction is cheap
// next to the API round trips these tools make.
func NewServer(token string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "enact-google-workspace",
		Version: "1.0.0",
	}, nil)

	c := newClient(token)
	registerGmail(server, c)
	registerCalendar(server, c)
	registerDrive(server, c)
	registerDocs(server, c)
	registerSheets(server, c)
	registerSlides(server, c)
	return server
}

// readOnly and mutating describe a tool's effect, so a client can warn before
// running one. Taken from the annotations the Python reference server sets.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		IdempotentHint:  true,
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(true),
	}
}

// mutating marks a tool that changes the user's data. destructive is true for
// the ones that remove or overwrite rather than add — a distinction a UI can
// use to ask before proceeding.
func mutating(destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		IdempotentHint:  false,
		DestructiveHint: boolPtr(destructive),
		OpenWorldHint:   boolPtr(true),
	}
}

func boolPtr(b bool) *bool { return &b }

// result renders a tool's answer: a text summary the model reads, plus the
// structured payload for a client that wants the detail.
func result(summary string, structured any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, structured, nil
}

// fail turns an error into a TOOL error rather than a protocol error.
//
// The distinction matters: a protocol error aborts the call and the model
// never learns why, while a tool error reaches it as a result it can act on
// and relay — "the credential is missing", "that document does not exist".
func fail(err error) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}, nil, nil
}

// addTool registers one tool, converting any error it returns into a TOOL
// error so no handler has to remember to.
func addTool[T any](server *mcp.Server, tool *mcp.Tool, run func(ctx context.Context, args T) (string, any, error)) {
	mcp.AddTool[T, any](server, tool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args T) (*mcp.CallToolResult, any, error) {
			summary, structured, err := run(ctx, args)
			if err != nil {
				return fail(err)
			}
			return result(summary, structured)
		})
}

// truncate keeps a body readable in a model's context. Google documents and
// long mail threads are unbounded; a tool result that fills the window is
// worse than one that says it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n… [truncated, %d bytes total]", len(s))
}

// jsonString renders a value compactly for a text summary.
func jsonString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// joinNonEmpty is strings.Join over the values that are actually set.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
