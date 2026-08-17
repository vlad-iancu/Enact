package enacttoolregistry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/mcp"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// serverIDPattern constrains server ids: user-friendly, URL-safe, unique.
var serverIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// RegistryAPI serves the MCP server registry and the tool-cache queries,
// plus the MCP proxy routes.
type RegistryAPI struct {
	repo        *tools.Repository
	mcpTimeout  time.Duration
	proxyClient *http.Client
	// probeClient reaches third-party MCP servers directly for the
	// platform's own handshake and tool listing; identities resolves the
	// credentials a server's owner configured for it.
	probeClient *http.Client
	identities  *extidentities.Client
	rbac        *rbac.Client
	enforcer    *rbac.Enforcer
	logger      *logging.Logger
}

type createServerRequest struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	TransportType string `json:"transport_type"`
	Description   string `json:"description"`
	Owner         string `json:"owner"`
	// Per-tool credential configuration: what each tool needs, and how it
	// is presented to the server.
	ToolAccessRequirements map[string][]tools.AccessRequirement `json:"tool_access_requirements"`
	ToolAuthorizations     map[string]tools.ToolAuthorization   `json:"tool_authorizations"`
}

// updateServerRequest carries the updatable fields; absent fields (nil)
// keep their current values. The id is fixed at creation.
type updateServerRequest struct {
	URL           *string `json:"url"`
	TransportType *string `json:"transport_type"`
	Description   *string `json:"description"`
	// Pointer-to-map so an absent field keeps the current configuration
	// while an explicit {} clears it.
	ToolAccessRequirements *map[string][]tools.AccessRequirement `json:"tool_access_requirements"`
	ToolAuthorizations     *map[string]tools.ToolAuthorization   `json:"tool_authorizations"`
}

type serversResponse struct {
	Servers []tools.Server `json:"servers"`
}

type toolsResponse struct {
	Tools []tools.Tool `json:"tools"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// WebServices returns the registry route group.
func (a *RegistryAPI) WebServices() []*restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/servers").Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/create").
		To(a.createServer).
		Consumes(restful.MIME_JSON).
		Reads(createServerRequest{}).
		Doc("Register an MCP server: health-checks the endpoint, caches its tools, stores the record").
		Returns(http.StatusCreated, "Registered", tools.Server{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "Server id already registered", errorResponse{}).
		Returns(http.StatusBadGateway, "MCP server unreachable", errorResponse{}))

	ws.Route(ws.PUT("/update").
		To(a.updateServer).
		Consumes(restful.MIME_JSON).
		Reads(updateServerRequest{}).
		Param(ws.QueryParameter("id", "server id").Required(true)).
		Doc("Update an MCP server: absent fields keep their values; a changed url or transport is re-checked and the tool cache refreshed").
		Returns(http.StatusOK, "Updated", tools.Server{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "No such server", errorResponse{}).
		Returns(http.StatusBadGateway, "MCP server unreachable", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.listServers).
		Param(ws.QueryParameter("ids", "server ids to filter by (repeatable)").AllowMultiple(true)).
		Param(ws.QueryParameter("owner", "filter by owning user id")).
		Doc("List registered MCP servers, newest first").
		Returns(http.StatusOK, "OK", serversResponse{}))

	ws.Route(ws.DELETE("").
		To(a.deleteServer).
		Param(ws.QueryParameter("id", "server id").Required(true)).
		Doc("Delete an MCP server and its cached tools").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "No such server", errorResponse{}))

	ws.Route(ws.GET("/tools").
		To(a.listTools).
		Param(ws.QueryParameter("ids", "server ids to filter by (repeatable)").AllowMultiple(true)).
		Doc("List cached tool definitions across servers").
		Returns(http.StatusOK, "OK", toolsResponse{}))

	// MCP proxy: spec-compliant pass-through to the underlying server, so
	// real MCP clients can point at the platform. The catch-all forwards
	// the session paths the SSE transport advertises via endpoint events.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		ws.Route(ws.Method(method).Path("/{server-id}/mcp").
			To(a.proxyMCP).
			Doc("MCP streamable-http proxy to the registered server").
			Consumes("*/*"))
		ws.Route(ws.Method(method).Path("/{server-id}/sse").
			To(a.proxySSE).
			Doc("MCP SSE proxy to the registered server (endpoint events rewritten through the proxy)").
			Consumes("*/*"))
		ws.Route(ws.Method(method).Path("/{server-id}/{subpath:*}").
			To(a.proxySubpath).
			Doc("Proxy for transport session paths (e.g. the SSE message endpoint)").
			Consumes("*/*"))
	}

	return []*restful.WebService{ws}
}

func (a *RegistryAPI) createServer(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("server registration requested")
	if err := a.enforcer.Require(req.Request.Context(), rbac.Permission(rbac.ResourceMCPServer, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("server registration denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}

	var body createServerRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.Owner == "" {
		body.Owner = identity.FromContext(req.Request.Context())
	}
	// Template text is credential-adjacent: log shape, never content.
	logger.Info("registration decoded", "id", body.ID, "url", body.URL, "transport", body.TransportType,
		"owner", body.Owner, "tools_with_requirements", len(body.ToolAccessRequirements),
		"tools_with_authorizations", len(body.ToolAuthorizations))

	if msg, ok := validateServerFields(body.ID, body.URL, body.TransportType); !ok {
		logger.Warn("registration rejected", "reason", msg)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, msg)
		return
	}
	if err := tools.ValidateToolAuthorization(body.ToolAccessRequirements, body.ToolAuthorizations); err != nil {
		logger.Warn("registration rejected: invalid tool authorization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	// The id is unique within an ORGANIZATION, not platform-wide: the same
	// name may be registered in two organizations, and neither refusal
	// discloses the other's existence.
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("register server: no organization", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	_, exists, err := a.repo.GetServer(req.Request.Context(), organizationID, body.ID)
	if err != nil {
		logger.Error("failed to check existing server", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register server")
		return
	}
	if exists {
		logger.Warn("server id already registered", "id", body.ID)
		requesthelper.WriteError(req, resp, http.StatusConflict, fmt.Sprintf("server id %q is already registered", body.ID))
		return
	}

	now := time.Now().UTC()
	server := tools.Server{
		ID:                     body.ID,
		OrganizationID:         organizationID,
		URL:                    body.URL,
		TransportType:          body.TransportType,
		Description:            body.Description,
		Owner:                  body.Owner,
		ToolAccessRequirements: body.ToolAccessRequirements,
		ToolAuthorizations:     body.ToolAuthorizations,
		CreatedAt:              now,
		UpdatedAt:              now,
	}

	// The server must be alive to register: connect, initialize, and pull
	// its tool list in one probe — presenting the owner's credentials when
	// the server authenticates its handshake.
	defs, err := a.probeTools(req.Request.Context(), server)
	if err != nil {
		logger.Warn("mcp server probe failed", "url", body.URL, "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, fmt.Sprintf("MCP server could not be probed: %v", err))
		return
	}
	server.ToolCount = len(defs)
	logger.Info("mcp server probed", "id", body.ID, "tool_count", len(defs))
	warnUnknownConfiguredTools(logger, server, defs)
	// The registrant owns it.
	if err := a.rbac.Grant(req.Request.Context(), rbac.GrantRequest{
		UserID:     server.Owner,
		Resource:   rbac.ResourceMCPServer,
		ResourceID: server.ID,
	}); err != nil {
		logger.Error("failed to record ownership", "id", server.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record ownership of the server")
		return
	}
	// Drop the owner's cached rules, which predate this grant — see
	// rbac.Enforcer.Forget.
	a.enforcer.Forget(server.Owner)
	if err := a.repo.SaveServer(req.Request.Context(), server); err != nil {
		logger.Error("failed to store server", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to register server")
		return
	}
	if err := a.repo.ReplaceTools(req.Request.Context(), server.OrganizationID, server.ID, defs); err != nil {
		logger.Error("failed to cache tools", "id", server.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to cache tools")
		return
	}
	logger.Info("server registered", "id", server.ID, "tool_count", len(defs))
	requesthelper.WriteJSON(req, resp, http.StatusCreated, server)
}

func (a *RegistryAPI) updateServer(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.QueryParameter("id")
	logger.Info("server update requested", "id", id)

	var body updateServerRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid update body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("update decoded", "id", id,
		"url_provided", body.URL != nil,
		"transport_provided", body.TransportType != nil,
		"description_provided", body.Description != nil,
		"requirements_provided", body.ToolAccessRequirements != nil,
		"authorizations_provided", body.ToolAuthorizations != nil)

	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceMCPServer, rbac.ActionEdit, id); err != nil {
		logger.Warn("server update denied", "err", err)
		rbac.WriteDenied(req, resp, err, fmt.Sprintf("server %q not found", id))
		return
	}
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("no organization", "err", err)
		rbac.WriteDenied(req, resp, err, fmt.Sprintf("server %q not found", id))
		return
	}
	// Looking up by (organization, id) is what keeps another organization's
	// server invisible: it is simply not found, with no separate check to
	// forget.
	server, found, err := a.repo.GetServer(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to load server", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to update server")
		return
	}
	if !found {
		logger.Warn("update for unknown server", "id", id)
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("server %q not found", id))
		return
	}

	probeConfigBefore := probeFingerprint(server)
	endpointChanged := false
	if body.URL != nil && *body.URL != server.URL {
		server.URL = *body.URL
		endpointChanged = true
	}
	if body.TransportType != nil && *body.TransportType != server.TransportType {
		server.TransportType = *body.TransportType
		endpointChanged = true
	}
	if body.Description != nil {
		server.Description = *body.Description
	}
	if body.ToolAccessRequirements != nil {
		server.ToolAccessRequirements = *body.ToolAccessRequirements
	}
	if body.ToolAuthorizations != nil {
		server.ToolAuthorizations = *body.ToolAuthorizations
	}
	if msg, ok := validateServerFields(server.ID, server.URL, server.TransportType); !ok {
		logger.Warn("update rejected", "reason", msg)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, msg)
		return
	}
	// Validate the MERGED result: changing only the requirements must be
	// checked against the authorizations already stored, and vice versa.
	if err := tools.ValidateToolAuthorization(server.ToolAccessRequirements, server.ToolAuthorizations); err != nil {
		logger.Warn("update rejected: invalid tool authorization", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}

	// A changed endpoint must prove itself like a new registration, and its
	// cached tools are stale by definition. So must changed probe
	// credentials: fixing them is exactly how a gated server that failed to
	// register gets in, and it must be proved rather than assumed.
	if endpointChanged || probeFingerprint(server) != probeConfigBefore {
		defs, err := a.probeTools(req.Request.Context(), server)
		if err != nil {
			logger.Warn("mcp server probe failed after update", "url", server.URL, "err", err)
			requesthelper.WriteError(req, resp, http.StatusBadGateway, fmt.Sprintf("MCP server could not be probed: %v", err))
			return
		}
		if err := a.repo.ReplaceTools(req.Request.Context(), server.OrganizationID, server.ID, defs); err != nil {
			logger.Error("failed to refresh tool cache", "id", server.ID, "err", err)
			requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to refresh tools")
			return
		}
		server.ToolCount = len(defs)
		logger.Info("tool cache refreshed on update", "id", server.ID, "tool_count", len(defs))
	}

	server.UpdatedAt = time.Now().UTC()
	if err := a.repo.SaveServer(req.Request.Context(), server); err != nil {
		logger.Error("failed to store server update", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to update server")
		return
	}
	logger.Info("server updated", "id", server.ID, "endpoint_changed", endpointChanged)
	requesthelper.WriteJSON(req, resp, http.StatusOK, server)
}

func (a *RegistryAPI) listServers(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	ids := req.Request.URL.Query()["ids"]
	owner := req.QueryParameter("owner")
	logger.Info("server list requested", "ids", ids, "owner", owner)

	// owner set means "a person is browsing their servers": widen the
	// candidates to their organization and let their rules decide. Left
	// empty it stays a service-side lookup (the agent runtime resolving ids,
	// the refresh sweep), which carries no user to authorize.
	var organizationID string
	var effective rbac.Effective
	if owner != "" {
		var err error
		if organizationID, err = a.enforcer.Organization(req.Request.Context()); err != nil {
			rbac.WriteDeniedForbidden(req, resp, err)
			return
		}
		if effective, err = a.enforcer.CallerEffective(req.Request.Context()); err != nil {
			rbac.WriteDeniedForbidden(req, resp, err)
			return
		}
	}

	servers, err := a.repo.ListServers(req.Request.Context(), ids, organizationID)
	if err != nil {
		logger.Error("failed to list servers", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list servers")
		return
	}
	if owner != "" {
		visible := make([]tools.Server, 0, len(servers))
		for _, s := range servers {
			if effective.Allows(rbac.Permission(rbac.ResourceMCPServer, rbac.ActionView, s.ID)) {
				visible = append(visible, s)
			}
		}
		logger.Info("servers listed", "candidates", len(servers), "visible", len(visible))
		servers = visible
	} else {
		logger.Info("servers listed", "count", len(servers))
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, serversResponse{Servers: servers})
}

func (a *RegistryAPI) deleteServer(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	id := req.QueryParameter("id")
	logger.Info("server deletion requested", "id", id)
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceMCPServer, rbac.ActionDelete, id); err != nil {
		logger.Warn("server deletion denied", "err", err)
		rbac.WriteDenied(req, resp, err, fmt.Sprintf("server %q not found", id))
		return
	}

	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		logger.Warn("no organization", "err", err)
		rbac.WriteDenied(req, resp, err, fmt.Sprintf("server %q not found", id))
		return
	}
	server, found, err := a.repo.GetServer(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to load server", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete server")
		return
	}
	if !found {
		logger.Warn("deletion for unknown server", "id", id)
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("server %q not found", id))
		return
	}
	if err := a.repo.DeleteTools(req.Request.Context(), organizationID, id); err != nil {
		logger.Error("failed to delete cached tools", "id", id, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete server")
		return
	}
	if err := a.repo.DeleteServer(req.Request.Context(), organizationID, id); err != nil {
		logger.Error("failed to delete server", "id", id, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete server")
		return
	}
	// The ownership rule outlives nothing: drop it with the server.
	if err := a.rbac.Revoke(req.Request.Context(), rbac.GrantRequest{
		UserID:     server.Owner,
		Resource:   rbac.ResourceMCPServer,
		ResourceID: id,
	}); err != nil {
		logger.Warn("failed to revoke ownership; the rule now points at nothing", "id", id, "err", err)
	}
	// And again on the way out, so the rule stops being honoured now
	// rather than at the end of the TTL.
	a.enforcer.Forget(server.Owner)
	logger.Info("server deleted", "id", id)
	resp.WriteHeader(http.StatusNoContent)
}

func (a *RegistryAPI) listTools(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	ids := req.Request.URL.Query()["ids"]
	organizationID := req.QueryParameter("organization_id")
	logger.Info("tool list requested", "ids", ids, "organization_id", organizationID)

	// The organization is a parameter rather than the caller's, because the
	// inference tool loop asks on behalf of an agent: the organization that
	// matters is the AGENT's, not whichever identity the request carries.
	// Empty means unfiltered, for callers that already hold qualified ids.
	defs, err := a.repo.ListTools(req.Request.Context(), organizationID, ids)
	if err != nil {
		logger.Error("failed to list tools", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list tools")
		return
	}
	logger.Info("tools listed", "count", len(defs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, toolsResponse{Tools: defs})
}

// probeFingerprint summarizes the configuration the platform's own probe
// uses, so an update can tell whether it changed. Comparing the rendered
// JSON of just those keys keeps an unrelated edit — a tool's params, say —
// from forcing a live round trip to a third party.
func probeFingerprint(server tools.Server) string {
	shape := struct {
		Requirements   map[string][]tools.AccessRequirement `json:"r"`
		Authorizations map[string]tools.ToolAuthorization   `json:"a"`
	}{
		Requirements:   map[string][]tools.AccessRequirement{},
		Authorizations: map[string]tools.ToolAuthorization{},
	}
	for _, key := range []string{tools.ProbeInitialize, tools.ProbeListTools} {
		if reqs, ok := server.ToolAccessRequirements[key]; ok {
			shape.Requirements[key] = reqs
		}
		if auth, ok := server.ToolAuthorizations[key]; ok {
			shape.Authorizations[key] = auth
		}
	}
	// Map key order is not stable in Go, but there are at most two keys and
	// encoding/json sorts them, so this is deterministic.
	raw, err := json.Marshal(shape)
	if err != nil {
		return ""
	}
	return string(raw)
}

// validateServerFields checks the id/url/transport trio shared by create
// and update.
func validateServerFields(id, rawURL, transport string) (string, bool) {
	if !serverIDPattern.MatchString(id) {
		return "id must be 1-64 characters: letters, digits, '-' or '_', starting alphanumeric", false
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "url must be a valid http(s) URL", false
	}
	if !mcp.ValidTransport(transport) {
		return fmt.Sprintf("transport_type must be %q or %q", mcp.TransportStreamableHTTP, mcp.TransportSSE), false
	}
	return "", true
}

// warnUnknownConfiguredTools flags credential configuration naming tools the
// server did not advertise. It is a warning, not an error: a server's tool
// set changes between refreshes, so rejecting would make a legitimate
// configuration unregisterable.
func warnUnknownConfiguredTools(logger *logging.Logger, server tools.Server, defs []mcp.Tool) {
	if len(server.ToolAccessRequirements) == 0 {
		return
	}
	advertised := make(map[string]bool, len(defs))
	for _, def := range defs {
		advertised[def.Name] = true
	}
	var unknown []string
	for name := range server.ToolAccessRequirements {
		// The wildcard names every tool by construction, and the probe keys
		// name none, so neither can be a tool the server fails to advertise.
		if name != tools.WildcardTool && !tools.IsProbeKey(name) && !advertised[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		logger.Warn("credential configuration names tools this server does not advertise",
			"id", server.ID, "tools", unknown)
	}
}
