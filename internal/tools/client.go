package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/mcp"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling the enact-tool-registry
// service.
type ClientConfig struct {
	// BaseURL is the root URL of the tool registry service.
	BaseURL string `env:"TOOL_REGISTRY_API_URL, default=http://localhost:8007"`

	// Timeout bounds each management call end to end. Tool calls through
	// the MCP proxy use CallTimeout instead — tools may legitimately be
	// slow.
	Timeout time.Duration `env:"TOOL_REGISTRY_API_TIMEOUT, default=10s"`
	// CallTimeout bounds one MCP tool invocation through the proxy.
	CallTimeout time.Duration `env:"TOOL_REGISTRY_CALL_TIMEOUT, default=60s"`
}

// Client is the HTTP wrapper other services use to talk to the tool
// registry — the source of truth for MCP servers and the tool cache. It
// lives in the tools domain package so callers depend on the domain, never
// on another service's internals.
type Client struct {
	http    *http.Client
	mcpHTTP *http.Client
	baseURL string
}

// NewClient returns a Client for the registry at cfg.BaseURL. base is the
// caller's S2S signing transport (nil = http.DefaultTransport); the tracing
// transport is layered on top.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	transport := requesthelper.NewTransport(base)
	return &Client{
		http: &http.Client{Transport: transport, Timeout: cfg.Timeout},
		// identityTransport wraps the S2S transport so MCP traffic carries the
		// caller as well as the service. The proxy resolves a server within
		// the caller's organization, so without it every proxied tool call
		// arrives anonymous and resolves nothing.
		mcpHTTP: &http.Client{Transport: &identityTransport{base: transport}, Timeout: cfg.CallTimeout},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// CreateServerRequest is the body of a server registration.
type CreateServerRequest struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	TransportType string `json:"transport_type"`
	Description   string `json:"description"`
	// Owner is set by enact-main to the acting user's id; services calling
	// on their own behalf may leave it empty.
	Owner string `json:"owner,omitempty"`
	// Per-tool credential configuration.
	ToolAccessRequirements map[string][]AccessRequirement `json:"tool_access_requirements,omitempty"`
	ToolAuthorizations     map[string]ToolAuthorization   `json:"tool_authorizations,omitempty"`
}

// do issues one JSON request. 404 maps to found=false, 400 to
// *requesthelper.BadRequestError, 409 to a ConflictError.
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, fmt.Errorf("tools: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("tools: %s %s: %w", method, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case wantStatus:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return false, fmt.Errorf("tools: decode response: %w", err)
			}
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusBadRequest, http.StatusConflict, http.StatusBadGateway, http.StatusForbidden:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusForbidden {
			return false, &requesthelper.ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("tools: %s %s: unexpected status %d", method, endpoint, resp.StatusCode)
	}
}

// Create registers an MCP server; the registry health-checks the endpoint
// and caches its tools before answering.
func (c *Client) Create(ctx context.Context, body CreateServerRequest) (Server, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Server{}, fmt.Errorf("tools: marshal create request: %w", err)
	}
	var created Server
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/servers/create", payload, http.StatusCreated, &created); err != nil {
		return Server{}, err
	}
	return created, nil
}

// Update partially updates a server. rawBody passes through verbatim to
// preserve absent-field semantics. The boolean reports existence.
func (c *Client) Update(ctx context.Context, id string, rawBody json.RawMessage) (Server, bool, error) {
	var updated Server
	found, err := c.do(ctx, http.MethodPut, c.baseURL+"/v1/servers/update?id="+url.QueryEscape(id), rawBody, http.StatusOK, &updated)
	if err != nil {
		return Server{}, false, err
	}
	return updated, found, nil
}

// Delete removes a server and its cached tools. The boolean reports
// existence.
func (c *Client) Delete(ctx context.Context, id string) (bool, error) {
	return c.do(ctx, http.MethodDelete, c.baseURL+"/v1/servers?id="+url.QueryEscape(id), nil, http.StatusNoContent, nil)
}

// List returns servers, optionally filtered by ids and/or owner.
func (c *Client) List(ctx context.Context, ids []string, owner string) ([]Server, error) {
	params := url.Values{}
	for _, id := range ids {
		params.Add("ids", id)
	}
	if owner != "" {
		params.Set("owner", owner)
	}
	endpoint := c.baseURL + "/v1/servers"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	var out struct {
		Servers []Server `json:"servers"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Servers, nil
}

// Get fetches one server by id. The boolean reports existence.
func (c *Client) Get(ctx context.Context, id string) (Server, bool, error) {
	servers, err := c.List(ctx, []string{id}, "")
	if err != nil {
		return Server{}, false, err
	}
	if len(servers) == 0 {
		return Server{}, false, nil
	}
	return servers[0], true, nil
}

// ListTools returns the cached tool definitions, optionally filtered to the
// given server ids.
func (c *Client) ListTools(ctx context.Context, organizationID string, serverIDs []string) ([]Tool, error) {
	params := url.Values{}
	if organizationID != "" {
		params.Set("organization_id", organizationID)
	}
	for _, id := range serverIDs {
		params.Add("ids", id)
	}
	endpoint := c.baseURL + "/v1/servers/tools"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// ProxyURL is the registry's MCP-compliant endpoint for one server: an MCP
// client pointed here talks to the underlying server through the registry.
func (c *Client) ProxyURL(server Server) string {
	path := "/mcp"
	if server.TransportType == mcp.TransportSSE {
		path = "/sse"
	}
	return c.baseURL + "/v1/servers/" + url.PathEscape(server.ID) + path
}

// CallOptions carries per-invocation credential material.
type CallOptions struct {
	// Headers are the headers the UPSTREAM MCP server must see. They travel
	// to the registry inside HeaderToolAuth because the S2S transport owns
	// Authorization on this hop; the proxy sets them on the third-party
	// request.
	Headers map[string]string
}

// CallTool invokes one tool on a registered server through the registry's
// MCP proxy: connect, call, close. The session is short-lived by design —
// the inference tool loop makes isolated calls.
//
// NOTE: this client's timeout (TOOL_REGISTRY_CALL_TIMEOUT) bounds the whole
// exchange, so anything that may block for a long time — notably waiting
// for a user to authorize — must happen BEFORE this call, not inside it.
func (c *Client) CallTool(ctx context.Context, server Server, name string, arguments json.RawMessage, opts CallOptions) (mcp.CallResult, error) {
	httpClient := c.mcpHTTP
	if len(opts.Headers) > 0 {
		encoded, err := EncodeAuthHeaders(opts.Headers)
		if err != nil {
			return mcp.CallResult{}, err
		}
		// A per-call client: c.mcpHTTP is shared across calls and must not
		// be mutated. Setting the header on the transport covers every
		// request of the session (initialize, tools/call, the SSE stream),
		// which is what a server authenticating at initialize requires.
		httpClient = &http.Client{
			Transport: &authHeaderTransport{base: c.mcpHTTP.Transport, value: encoded},
			Timeout:   c.mcpHTTP.Timeout,
		}
	}
	session, err := mcp.Connect(ctx, c.ProxyURL(server), server.TransportType, httpClient)
	if err != nil {
		return mcp.CallResult{}, err
	}
	defer func() { _ = session.Close() }()
	return session.CallTool(ctx, name, arguments)
}

// identityTransport carries the calling USER on every request of an MCP
// session, taken from the request context.
//
// The service identity is signed by the S2S transport underneath; this is the
// person on whose behalf the call is made, which is what the registry's proxy
// resolves the server's organization from. A session issues several requests
// (initialize, tools/call, the stream), and all of them need it — hence a
// transport rather than a header set once.
type identityTransport struct{ base http.RoundTripper }

func (t *identityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if user := identity.FromContext(r.Context()); user != "" {
		r = r.Clone(r.Context())
		r.Header.Set(identity.Header, user)
	}
	return base.RoundTrip(r)
}

// authHeaderTransport attaches the tool-auth envelope to every request of
// one MCP session.
type authHeaderTransport struct {
	base  http.RoundTripper
	value string
}

func (t *authHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	r = r.Clone(r.Context())
	r.Header.Set(HeaderToolAuth, t.value)
	return base.RoundTrip(r)
}
