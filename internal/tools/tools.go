// Package tools holds the MCP tool domain: registered MCP servers
// (enact-tool-servers) and the cached tool definitions harvested from them
// (enact-tool-cache). It is a standalone package because several services
// share the domain: the tool registry owns the records and the cache, the
// agent API validates server references, the inference service resolves
// tools for Bedrock, and enact-main manages servers on behalf of users.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/mcp"
	"enact/internal/opensearch"
)

// Config holds the OpenSearch index names for the tool domain.
type Config struct {
	ServersIndex string `env:"OPENSEARCH_INDEX_TOOL_SERVERS, default=enact-tool-servers"`
	CacheIndex   string `env:"OPENSEARCH_INDEX_TOOL_CACHE, default=enact-tool-cache"`
}

// Server is a registered MCP server: a user-chosen unique id, the endpoint
// where the MCP protocol is served, and which transport it speaks.
type Server struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	TransportType string `json:"transport_type"`
	Description   string `json:"description"`
	// Owner is the user id of the creator; server management in enact-main
	// is scoped to it.
	Owner     string `json:"owner"`
	ToolCount int    `json:"tool_count"`

	// ToolAccessRequirements declares, per tool, which third-party
	// credentials the tool needs. Keyed by the MCP tool's own name (not the
	// sanitized name the model sees), or by WildcardTool for every tool the
	// server offers. At most one access level per provider per tool.
	ToolAccessRequirements map[string][]AccessRequirement `json:"tool_access_requirements,omitempty"`
	// ToolAuthorizations declares, per tool, how those credentials are
	// presented to the server: as HTTP headers, as call arguments, or both.
	// WildcardTool applies to every tool here too.
	ToolAuthorizations map[string]ToolAuthorization `json:"tool_authorizations,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AccessRequirement is one provider a tool needs a credential from,
// optionally at a named access level. The level is passed straight to the
// identity service's required_access, which resolves level names against the
// provider record.
//
// AccessLevel may be empty, and then the requirement is only "the user must
// have connected this provider" — no coverage check. That is the whole
// contract for a credential with nothing to say about scope: an API key that
// either works or does not.
type AccessRequirement struct {
	Provider    string `json:"provider"`
	AccessLevel string `json:"access_level,omitempty"`
}

// RequiredAccess is what to send as the identity service's required_access.
// Nil when no level is named — never []string{""}, which would ask the
// service to check coverage of a scope nobody has.
func (r AccessRequirement) RequiredAccess() []string {
	if r.AccessLevel == "" {
		return nil
	}
	return []string{r.AccessLevel}
}

// HeaderAuthorization renders one HTTP header sent to the MCP server.
type HeaderAuthorization struct {
	HeaderName     string `json:"header_name"`
	HeaderTemplate string `json:"header_template"`
}

// ParamAuthorization renders one argument merged into the tool call.
type ParamAuthorization struct {
	ParamName     string `json:"param_name"`
	ParamTemplate string `json:"param_template"`
}

// ToolAuthorization is how one tool's credentials reach the MCP server. A
// tool may use headers, params, or both.
type ToolAuthorization struct {
	HeadersAuthorization []HeaderAuthorization `json:"headers_authorization,omitempty"`
	ParamAuthorization   []ParamAuthorization  `json:"param_authorization,omitempty"`
}

// WildcardTool is the tool_access_requirements / tool_authorizations key
// that stands for every tool a server offers — the common case, where one
// server speaks to one third party and every tool needs the same credential.
//
// A key naming a tool outright beats the wildcard for that tool, and beats
// it WHOLE: the specific entry replaces the wildcard's rather than adding to
// it, so what a tool sends is always one list, read from one place. A tool
// that needs the wildcard's credentials plus its own restates both.
const WildcardTool = "*"

// The probe keys configure how the PLATFORM ITSELF reaches a server, rather
// than what one of its tools needs. A server that authenticates its
// handshake — GitHub's does, with a 401 before the JSON-RPC body is even
// parsed — cannot be registered, refreshed or opened without them.
//
// They lead with "/" precisely because an MCP tool name never does, so they
// cannot collide with a real tool. They are resolved against the server's
// OWNER, everywhere: at registration, on the background refresh where no
// user exists at all, and inside a caller's agent session. They are the key
// to the door, not the identity of whoever walks through it.
const (
	// ProbeInitialize covers the MCP handshake that opens any session.
	ProbeInitialize = "/initialize"
	// ProbeListTools covers the tools/list call the catalogue is built from.
	// When only one of the two is declared it covers both phases.
	ProbeListTools = "/list-tools"
)

// IsProbeKey reports whether a configuration key addresses the platform's
// own connection rather than a tool. Callers listing a server's tools must
// skip these, or they surface to users as tools that do not exist.
func IsProbeKey(key string) bool {
	return key == ProbeInitialize || key == ProbeListTools
}

// Requirements returns the credential requirements of one tool: its own if
// it declares any, else the wildcard's.
func (s Server) Requirements(toolName string) []AccessRequirement {
	if own, ok := s.ToolAccessRequirements[toolName]; ok {
		return own
	}
	if IsProbeKey(toolName) {
		// The wildcard is about tools. Letting a probe inherit it would make
		// registering any wildcard server quietly start spending the owner's
		// credential on platform traffic.
		return nil
	}
	return s.ToolAccessRequirements[WildcardTool]
}

// Authorization returns how one tool's credentials are presented, and
// whether anything is configured at all. Like Requirements, a tool's own
// entry wins outright over the wildcard's, and a probe key never inherits.
func (s Server) Authorization(toolName string) (ToolAuthorization, bool) {
	auth, ok := s.ToolAuthorizations[toolName]
	if !ok && !IsProbeKey(toolName) {
		auth, ok = s.ToolAuthorizations[WildcardTool]
	}
	if !ok || (len(auth.HeadersAuthorization) == 0 && len(auth.ParamAuthorization) == 0) {
		return ToolAuthorization{}, false
	}
	return auth, true
}

// Probe returns the requirements and authorization for one phase of the
// platform's own connection, falling back to the other phase when only one
// is declared — a server that gates its handshake gates everything after it,
// so configuring `/initialize` alone is the sane shorthand.
//
// Requirements and authorization are returned together because they are
// meaningless apart: the templates in one may only name providers from the
// other.
func (s Server) Probe(phase string) ([]AccessRequirement, ToolAuthorization, bool) {
	other := ProbeListTools
	if phase == ProbeListTools {
		other = ProbeInitialize
	}
	for _, key := range []string{phase, other} {
		if auth, ok := s.Authorization(key); ok {
			return s.Requirements(key), auth, true
		}
	}
	return nil, ToolAuthorization{}, false
}

// Tool is one cached tool definition, keyed by server id + tool name.
type Tool struct {
	ServerID string `json:"server_id"`
	mcp.Tool
	RefreshedAt time.Time `json:"refreshed_at"`
}

// Repository persists servers and the tool cache in OpenSearch.
type Repository struct {
	os      *opensearch.Client
	servers string
	cache   string
}

// NewRepository returns a Repository over the indices in cfg.
func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, servers: cfg.ServersIndex, cache: cfg.CacheIndex}
}

// EnsureIndices verifies both indices exist (created by infrastructure
// provisioning, never by services).
func (r *Repository) EnsureIndices(ctx context.Context) error {
	for _, index := range []string{r.servers, r.cache} {
		exists, err := r.os.IndexExists(ctx, index)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("tools: required index %q is missing; run `make infrastructure-up` to create it", index)
		}
	}
	return nil
}

// SaveServer persists a server record (create or full replace).
func (r *Repository) SaveServer(ctx context.Context, s Server) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.servers, s.ID, body)
}

// GetServer fetches a server by id. The boolean reports existence.
func (r *Repository) GetServer(ctx context.Context, id string) (Server, bool, error) {
	var s Server
	found, err := r.os.GetSource(ctx, r.servers, id, &s)
	return s, found, err
}

// DeleteServer removes a server record (its cached tools are deleted
// separately via DeleteTools).
func (r *Repository) DeleteServer(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.servers, id)
}

// ListServers returns servers, newest first, optionally filtered by ids
// and/or owner (empty means no filter).
func (r *Repository) ListServers(ctx context.Context, ids []string, owner string) ([]Server, error) {
	var filters []any
	if len(ids) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"id": ids}})
	}
	if owner != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"owner": owner}})
	}
	query := map[string]any{"match_all": map[string]any{}}
	if len(filters) > 0 {
		query = map[string]any{"bool": map[string]any{"filter": filters}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("tools: marshal list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.servers, body)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(hits))
	for _, h := range hits {
		var s Server
		if err := json.Unmarshal(h.Source, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ReplaceTools swaps the cached tool set of one server: stale entries are
// removed and the fresh definitions indexed, keyed "<server>::<tool>".
func (r *Repository) ReplaceTools(ctx context.Context, serverID string, defs []mcp.Tool) error {
	if err := r.DeleteTools(ctx, serverID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, def := range defs {
		doc := Tool{ServerID: serverID, Tool: def, RefreshedAt: now}
		body, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if err := r.os.IndexDoc(ctx, r.cache, serverID+"::"+def.Name, body); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTools removes every cached tool of one server.
func (r *Repository) DeleteTools(ctx context.Context, serverID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"server_id": serverID}},
	})
	if err != nil {
		return err
	}
	return r.os.DeleteByQuery(ctx, r.cache, body)
}

// ListTools returns cached tools, optionally filtered to the given server
// ids, sorted by server then name.
func (r *Repository) ListTools(ctx context.Context, serverIDs []string) ([]Tool, error) {
	query := map[string]any{"match_all": map[string]any{}}
	if len(serverIDs) > 0 {
		query = map[string]any{"terms": map[string]any{"server_id": serverIDs}}
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": query,
		"sort": []any{
			map[string]any{"server_id": map[string]any{"order": "asc"}},
			map[string]any{"name": map[string]any{"order": "asc"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("tools: marshal tools query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.cache, body)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(hits))
	for _, h := range hits {
		var t Tool
		if err := json.Unmarshal(h.Source, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
