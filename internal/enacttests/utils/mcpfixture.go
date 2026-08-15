package utils

import (
	"context"
	"net/http"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"enact/internal/logging"
)

// The MCP fixture is a real, spec-compliant MCP server embedded in the
// tests service: registry cases register it, and agent cases let the model
// call its tools. It stays deliberately tiny — a deterministic tool for
// zero-ambiguity assertions and an echo tool for argument round-trips.

// SecretNumber is what the fixture's secret_number tool returns; cases
// assert the model surfaces it.
const SecretNumber = "42"

// The authenticated tools below exist for the credential-injection cases:
// one gated on a header, one on a call argument. On the default path
// tools/list stays UNAUTHENTICATED, matching a server that gates its tools
// but not its handshake.
const (
	// FixtureAuthHeader is the header secret_number_authed demands.
	FixtureAuthHeader = "X-Fixture-Token"
	// FixtureMCPToken is the credential value the cases store.
	FixtureMCPToken = "e2e-mcp-secret-token"
	// FixtureAuthValue is what the header must carry once rendered.
	FixtureAuthValue = "Bearer " + FixtureMCPToken
)

// The GATED path models a server like GitHub's, which answers 401 at the
// transport before the JSON-RPC body is parsed — so `initialize` and
// `tools/list` are refused, not just tool calls. It is what the probe keys
// (tools.ProbeInitialize / ProbeListTools) exist for.
const (
	// GatedPath is appended to the fixture URL to reach the gated server.
	GatedPath = "/gated"
	// FixtureGateHeader admits a request to the gated path.
	FixtureGateHeader = "X-Fixture-Gate"
	// FixtureGateToken is the credential value the gate demands.
	FixtureGateToken = "e2e-mcp-gate-token"
)

// gate refuses a request that does not carry the gate header, the way a
// credential-gated MCP server refuses an anonymous handshake.
func gate(next http.Handler, logger *logging.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(FixtureGateHeader) != "Bearer "+FixtureGateToken {
			logger.Info("mcp fixture: refusing an unauthenticated request to the gated server",
				"path", r.URL.Path, "gate_header_present", r.Header.Get(FixtureGateHeader) != "")
			w.Header().Set("WWW-Authenticate", `Bearer realm="enact-test-fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type authedArgs struct {
	// APIKey is injected by the platform from the user's stored credential;
	// the model is told not to supply it.
	APIKey  string `json:"api_key" jsonschema:"injected by the platform - do not fill this in"`
	Message string `json:"message" jsonschema:"the text to echo back"`
}

type echoArgs struct {
	Message string `json:"message" jsonschema:"the text to echo back"`
}

type emptyArgs struct{}

// StartMCPFixture serves the fixture MCP server (streamable-http) on listen
// for the lifetime of the process.
func StartMCPFixture(listen string, logger *logging.Logger) {
	server := sdk.NewServer(&sdk.Implementation{Name: "enact-test-fixture", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{
		Name:        "secret_number",
		Description: "Returns the secret number. Call this whenever asked about the secret number.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: SecretNumber}},
		}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "echo",
		Description: "Echoes the given message back.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, args echoArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + args.Message}},
		}, nil, nil
	})

	// secret_number_authed: the HEADER credential path.
	sdk.AddTool(server, &sdk.Tool{
		Name:        "secret_number_authed",
		Description: "Returns the secret number, but only to an authenticated caller.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, any, error) {
		got := ""
		if req.Extra != nil {
			got = req.Extra.Header.Get(FixtureAuthHeader)
		}
		if got != FixtureAuthValue {
			logger.Warn("mcp fixture: rejecting unauthenticated call", "tool", "secret_number_authed", "header_present", got != "")
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: "missing or invalid " + FixtureAuthHeader}},
			}, nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: SecretNumber}}}, nil, nil
	})

	// echo_authed: the PARAM credential path.
	sdk.AddTool(server, &sdk.Tool{
		Name:        "echo_authed",
		Description: "Echoes the message back, but only when the api_key argument carries a valid credential.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, args authedArgs) (*sdk.CallToolResult, any, error) {
		if args.APIKey != FixtureMCPToken {
			logger.Warn("mcp fixture: rejecting call with a bad api_key", "tool", "echo_authed", "key_present", args.APIKey != "")
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: "missing or invalid api_key"}},
			}, nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + args.Message}}}, nil, nil
	})

	// header_probe reports which headers actually crossed the platform
	// boundary — the assertion surface for "the platform's own credentials
	// never reach a third party".
	sdk.AddTool(server, &sdk.Tool{
		Name:        "header_probe",
		Description: "Returns the sorted names of the HTTP headers this server received.",
	}, func(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, any, error) {
		var names []string
		if req.Extra != nil {
			for name := range req.Extra.Header {
				names = append(names, strings.ToLower(name))
			}
		}
		sort.Strings(names)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: strings.Join(names, ",")}},
		}, nil, nil
	})

	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	// The same server on two paths: open, and gated at the transport.
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.Handle(GatedPath, gate(handler, logger))
	mux.Handle(GatedPath+"/", gate(handler, logger))
	go func() {
		logger.Info("mcp fixture server listening", "addr", listen)
		if err := http.ListenAndServe(listen, mux); err != nil {
			logger.Error("mcp fixture server stopped", "err", err)
		}
	}()
}
