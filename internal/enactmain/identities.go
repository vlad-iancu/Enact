package enactmain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// identitiesWebService is the browser-facing surface over the external
// identity service: the accounts a user has connected at third parties.
//
// Every route is scoped to the SESSION's user — the identity service takes
// the user from the caller, and enact-main is the component that knows who
// the person actually is.
func (a *MainAPI) identitiesWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/identities").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.GET("").
		To(a.listConnectedIdentities).
		Doc("List the logged-in user's connected accounts (never their credentials)").
		Returns(http.StatusOK, "OK", connectedIdentitiesResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("/providers").
		To(a.listIdentityProviders).
		Doc("List the identity providers an account can be connected to").
		Returns(http.StatusOK, "OK", identityProvidersResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.POST("/pat").
		To(a.connectPATIdentity).
		Consumes(restful.MIME_JSON).
		Reads(connectPATRequest{}).
		Doc("Connect an account by pasting a personal access token").
		Returns(http.StatusCreated, "Connected", extidentities.IdentitySummary{}).
		Returns(http.StatusBadRequest, "Invalid request or unknown provider", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("/connect").
		To(a.connectOAuthIdentity).
		Param(ws.QueryParameter("provider", "provider name").Required(true)).
		Param(ws.QueryParameter("scopes", "scope to request (repeatable)").AllowMultiple(true)).
		Param(ws.QueryParameter("access_level", "a provider-defined access level to request instead of raw scopes")).
		Doc("Begin connecting an OAuth account: redirects the browser to the provider's consent screen").
		Returns(http.StatusFound, "Redirecting to the provider", nil).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.DELETE("/{provider}").
		To(a.disconnectIdentity).
		Param(ws.PathParameter("provider", "provider name")).
		Doc("Disconnect an account; every agent and MCP server that used it loses access").
		Returns(http.StatusNoContent, "Disconnected", nil).
		Returns(http.StatusNotFound, "Not connected", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	return ws
}

type connectedIdentitiesResponse struct {
	Identities []extidentities.IdentitySummary `json:"identities"`
}

type identityProvidersResponse struct {
	Providers []extidentities.ProviderSummary `json:"providers"`
}

type connectPATRequest struct {
	Provider string `json:"provider"`
	// AccessLevel labels what the token is believed to cover, using one of
	// the provider's declared levels.
	AccessLevel string `json:"access_level,omitempty"`
	Token       string `json:"token"`
	// Username is the second half of the pair for basic-auth providers
	// (JIRA Cloud: account email + API token).
	Username string `json:"username,omitempty"`
}

// identityCtx returns a context carrying the session user, so the identity
// service acts on the logged-in person and nobody else.
func identityCtx(req *restful.Request) *restful.Request {
	sess := sessionAttr(req)
	req.Request = req.Request.WithContext(identity.WithUserID(req.Request.Context(), sess.UserID))
	return req
}

func (a *MainAPI) listConnectedIdentities(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list connected identities requested")

	req = identityCtx(req)
	records, err := a.identities.List(req.Request.Context(), "")
	if err != nil {
		logger.Error("failed to list identities", "err", err)
		relayIdentitiesErr(req, resp, err, "list connected accounts")
		return
	}
	logger.Info("connected identities listed", "count", len(records))
	requesthelper.WriteJSON(req, resp, http.StatusOK, connectedIdentitiesResponse{Identities: records})
}

func (a *MainAPI) listIdentityProviders(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("list identity providers requested")

	req = identityCtx(req)
	providers, err := a.identities.Providers(req.Request.Context())
	if err != nil {
		logger.Error("failed to list identity providers", "err", err)
		relayIdentitiesErr(req, resp, err, "list identity providers")
		return
	}
	logger.Info("identity providers listed", "count", len(providers))
	requesthelper.WriteJSON(req, resp, http.StatusOK, identityProvidersResponse{Providers: providers})
}

func (a *MainAPI) connectPATIdentity(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("connect pat identity requested")

	var body connectPATRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid connect body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	// The token is a credential: never logged, only its presence.
	logger.Info("connect decoded", "provider", body.Provider,
		"access_level", body.AccessLevel, "token_present", body.Token != "", "username_present", body.Username != "")
	if body.Token == "" {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "token is required")
		return
	}

	credentials := map[string]any{"token": body.Token}
	if body.Username != "" {
		credentials["username"] = body.Username
	}
	req = identityCtx(req)
	stored, err := a.identities.Store(req.Request.Context(), extidentities.StoreIdentityRequest{
		Provider:    body.Provider,
		AccessLevel: body.AccessLevel,
		Credentials: credentials,
	})
	if err != nil {
		logger.Warn("failed to store identity", "err", err)
		relayIdentitiesErr(req, resp, err, "connect the account")
		return
	}
	logger.Info("identity connected", "provider", stored.Provider)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, stored)
}

func (a *MainAPI) connectOAuthIdentity(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	provider := req.QueryParameter("provider")
	scopes := req.Request.URL.Query()["scopes"]
	accessLevel := req.QueryParameter("access_level")
	logger.Info("oauth connect requested", "provider", provider,
		"scopes", scopes, "access_level", accessLevel)

	// Once the provider redirects back to the identity service, it sends
	// the browser here — the frontend when there is one, else the app page.
	returnTo := a.postLoginTarget()
	req = identityCtx(req)
	start, found, err := a.identities.Authorize(req.Request.Context(), provider, returnTo, scopes, accessLevel)
	if err != nil {
		logger.Warn("failed to start authorization", "err", err)
		relayIdentitiesErr(req, resp, err, "start the authorization")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("provider %q is not registered", provider))
		return
	}
	logger.Info("redirecting to provider consent", "provider", provider)
	http.Redirect(resp.ResponseWriter, req.Request, start.AuthorizationURL, http.StatusFound)
}

func (a *MainAPI) disconnectIdentity(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	provider := req.PathParameter("provider")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "provider", provider)
	logger.Info("disconnect identity requested")

	req = identityCtx(req)
	found, err := a.identities.Delete(req.Request.Context(), provider)
	if err != nil {
		logger.Error("failed to disconnect identity", "err", err)
		relayIdentitiesErr(req, resp, err, "disconnect the account")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "no connected account for this provider")
		return
	}
	logger.Info("identity disconnected")
	resp.WriteHeader(http.StatusNoContent)
}

// relayIdentitiesErr maps identity-service client errors onto user-facing
// replies: validation messages pass through as 400, everything else is 502.
func relayIdentitiesErr(req *restful.Request, resp *restful.Response, err error, action string) {
	var badReq *requesthelper.BadRequestError
	if errors.As(err, &badReq) {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, badReq.Message)
		return
	}
	// A conflict is a state of the world the caller can act on — "delete the
	// identities first", "that provider already exists" — and the identity
	// service wrote a message saying which. Relaying it as a bare 502 tells
	// an administrator only that something went wrong somewhere.
	var conflict *extidentities.ConflictError
	if errors.As(err, &conflict) {
		requesthelper.WriteError(req, resp, http.StatusConflict, conflict.Message)
		return
	}
	var gone *extidentities.ProviderGoneError
	if errors.As(err, &gone) {
		requesthelper.WriteError(req, resp, http.StatusFailedDependency, gone.Message)
		return
	}
	requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to "+action)
}

// ---------------------------------------------------------------------------
// Administration: registering the providers everyone else connects to
// ---------------------------------------------------------------------------

// registerAdminIdentityRoutes mounts provider administration on the admin
// web service, so it inherits the session + ADMIN_EMAIL gate.
//
// Creating a provider is a platform-wide act — it decides what every user
// can connect to, and for OAuth it stores this platform's client secret —
// so it is deliberately not an ordinary-user capability. Reading the
// provider list is (GET /identities/providers): users must see what they
// can connect to.
func (a *MainAPI) registerAdminIdentityRoutes(ws *restful.WebService) {
	ws.Route(ws.POST("/identity-providers/oauth").
		To(a.adminCreateOAuthProvider).
		Consumes(restful.MIME_JSON).
		Reads(extidentities.RegisterOAuthProviderRequest{}).
		Doc("Register an OAuth identity provider (admin only). Give discovery_url to fill the endpoints from the provider's well-known document, or state authorize_url and token_url explicitly").
		Returns(http.StatusCreated, "Registered", extidentities.ProviderSummary{}).
		Returns(http.StatusBadRequest, "Invalid request, or the provider is unreachable", errorResponse{}).
		Returns(http.StatusConflict, "A provider with that name already exists", errorResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))

	ws.Route(ws.POST("/identity-providers/pat").
		To(a.adminCreatePATProvider).
		Consumes(restful.MIME_JSON).
		Reads(extidentities.RegisterPATProviderRequest{}).
		Doc("Register a personal-access-token provider (admin only): a name is all it needs, since users paste their own tokens").
		Returns(http.StatusCreated, "Registered", extidentities.ProviderSummary{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusConflict, "A provider with that name already exists", errorResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))

	ws.Route(ws.DELETE("/identity-providers/{name}").
		To(a.adminDeleteProvider).
		Param(ws.PathParameter("name", "provider name")).
		Param(ws.QueryParameter("force", "also delete every user identity that references it").DataType("boolean")).
		Doc("Delete an identity provider (admin only); refused while stored identities still reference it unless force=true, which deletes those users' stored credentials too").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "No such provider", errorResponse{}).
		Returns(http.StatusConflict, "Identities still reference this provider; pass force=true", errorResponse{}).
		Returns(http.StatusForbidden, "Not the administrator", errorResponse{}))
}

func (a *MainAPI) adminCreateOAuthProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("admin_id", sessionAttr(req).UserID)
	logger.Info("admin create oauth provider requested")

	var body extidentities.RegisterOAuthProviderRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid oauth provider body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	// The client secret is a credential: log its presence, never its value.
	logger.Info("oauth provider decoded", "name", body.Name, "discovery_url", body.DiscoveryURL,
		"authorize_url", body.AuthorizeURL, "token_url", body.TokenURL, "client_id", body.ClientID,
		"client_secret_present", body.ClientSecret != "", "scopes", body.Scopes,
		"access_levels", len(body.AccessLevels), "use_pkce", body.UsePKCE)

	created, err := a.identities.RegisterOAuthProvider(identityCtx(req).Request.Context(), body)
	if err != nil {
		logger.Warn("oauth provider registration failed", "name", body.Name, "err", err)
		relayIdentitiesErr(req, resp, err, "register the provider")
		return
	}
	logger.Info("oauth provider registered", "name", created.Name,
		"authorize_url", created.AuthorizeURL, "token_url", created.TokenURL, "callback_url", created.CallbackURL)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) adminCreatePATProvider(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("admin_id", sessionAttr(req).UserID)
	logger.Info("admin create pat provider requested")

	var body extidentities.RegisterPATProviderRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid pat provider body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("pat provider decoded", "name", body.Name, "scheme", body.Scheme,
		"header_name", body.HeaderName, "access_levels", len(body.AccessLevels))

	created, err := a.identities.RegisterPATProvider(identityCtx(req).Request.Context(), body)
	if err != nil {
		logger.Warn("pat provider registration failed", "name", body.Name, "err", err)
		relayIdentitiesErr(req, resp, err, "register the provider")
		return
	}
	logger.Info("pat provider registered", "name", created.Name)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) adminDeleteProvider(req *restful.Request, resp *restful.Response) {
	name := req.PathParameter("name")
	force := req.QueryParameter("force") == "true"
	logger := requesthelper.Logger(req, a.logger).WithFields("admin_id", sessionAttr(req).UserID, "provider", name)
	logger.Info("admin delete provider requested", "force", force)

	found, err := a.identities.DeleteProvider(identityCtx(req).Request.Context(), name, force)
	if err != nil {
		logger.Warn("provider deletion failed", "err", err)
		relayIdentitiesErr(req, resp, err, "delete the provider")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("provider %q not found", name))
		return
	}
	logger.Info("provider deleted", "force", force)
	resp.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// "What does this agent need me to connect?"
// ---------------------------------------------------------------------------

// agentRequirement is one credential an agent's tools need, joined with
// whether the logged-in user has it.
//
// One entry per (provider, access level) across ALL of the agent's servers:
// an identity belongs to the provider, not to a server, so two servers
// wanting the same account are one thing for the user to connect, not two.
type agentRequirement struct {
	// Servers are the agent's MCP servers that need this credential, and
	// Tools the tools across them.
	Servers     []string `json:"servers"`
	Tools       []string `json:"tools"`
	Provider    string   `json:"provider"`
	AccessLevel string   `json:"access_level,omitempty"`
	Connected   bool     `json:"connected"`
	// Reason explains an unmet requirement: not_connected,
	// insufficient_access, or unavailable. Empty when connected.
	Reason string `json:"reason,omitempty"`
	// ProviderType and AccessLevels describe how to connect, so the UI can
	// choose between the OAuth redirect and a token form without a second
	// round trip.
	ProviderType string              `json:"provider_type,omitempty"`
	AccessLevels map[string][]string `json:"access_levels,omitempty"`
}

type agentRequirementsResponse struct {
	AgentID      string             `json:"agent_id"`
	Requirements []agentRequirement `json:"requirements"`
	// Satisfied is true when every requirement is connected — the one field
	// a "can I run this agent?" check needs.
	Satisfied bool `json:"satisfied"`
}

// agentRequiredIdentities answers what the logged-in user must connect
// before an agent's tools can run.
//
// The tool loop resolves exactly this at call time and emits
// toolCallWaitingAuthorization when something is missing; this endpoint
// exposes the same answer as a QUERY, so a UI can show "needs GitHub"
// up front instead of discovering it mid-conversation.
func (a *MainAPI) agentRequiredIdentities(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	agentID := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "agent_id", agentID)
	logger.Info("agent required identities requested")

	agent, found, err := a.agents.Get(req.Request.Context(), agentID)
	if err != nil {
		logger.Error("failed to load agent", "err", err)
		relayAgentErr(req, resp, err, "load the agent")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "agent not found")
		return
	}
	out := agentRequirementsResponse{AgentID: agent.ID, Requirements: []agentRequirement{}, Satisfied: true}
	if len(agent.Tools) == 0 {
		requesthelper.WriteJSON(req, resp, http.StatusOK, out)
		return
	}

	// By id, not by owner: an agent may reference a server someone else
	// registered, and the person running it still has to connect the
	// accounts its tools need.
	req = identityCtx(req)
	ctx := req.Request.Context()
	servers, err := a.toolRegistry.List(ctx, agent.Tools, "")
	if err != nil {
		logger.Error("failed to list the agent's mcp servers", "err", err)
		relayIdentitiesErr(req, resp, err, "resolve the agent's tools")
		return
	}
	providers, err := a.identities.Providers(ctx)
	if err != nil {
		// Provider metadata only enriches the answer; without it the UI can
		// still see what is missing.
		logger.Warn("failed to list identity providers", "err", err)
	}
	byName := make(map[string]extidentities.ProviderSummary, len(providers))
	for _, p := range providers {
		byName[p.Name] = p
	}

	// Collapse every server's per-tool requirements into one entry per
	// (provider, access level). The credential is shared, so asking the user
	// once is the honest prompt; two servers wanting DIFFERENT levels of one
	// provider stay two entries, and connecting both converges — an OAuth
	// re-authorization asks for the union of what is already granted.
	byServer := a.wildcardToolNames(ctx, logger, servers)
	order := []string{}
	merged := map[string]*agentRequirement{}
	for _, server := range servers {
		for toolName, reqs := range server.ToolAccessRequirements {
			// The probe keys are the platform's own connection to the server,
			// satisfied by its owner — not something the person running the
			// agent has to connect.
			if tools.IsProbeKey(toolName) {
				continue
			}
			// The wildcard is configuration, not a tool anyone can call:
			// report the tools it actually stands for.
			names := []string{toolName}
			if toolName == tools.WildcardTool {
				names = byServer[server.ID]
			}
			for _, r := range reqs {
				key := r.Provider + "\x00" + r.AccessLevel
				entry, seen := merged[key]
				if !seen {
					entry = &agentRequirement{Provider: r.Provider, AccessLevel: r.AccessLevel}
					merged[key] = entry
					order = append(order, key)
				}
				entry.Servers = appendUnique(entry.Servers, server.ID)
				for _, name := range names {
					entry.Tools = appendUnique(entry.Tools, name)
				}
			}
		}
	}
	for _, key := range order {
		entry := merged[key]
		sort.Strings(entry.Servers)
		sort.Strings(entry.Tools)
		if p, ok := byName[entry.Provider]; ok {
			entry.ProviderType = p.Type
			entry.AccessLevels = p.AccessLevels
		}
		entry.Connected, entry.Reason = a.credentialStatus(ctx, logger, *entry)
		if !entry.Connected {
			out.Satisfied = false
		}
		out.Requirements = append(out.Requirements, *entry)
	}
	sort.Slice(out.Requirements, func(i, j int) bool {
		if out.Requirements[i].Provider != out.Requirements[j].Provider {
			return out.Requirements[i].Provider < out.Requirements[j].Provider
		}
		return out.Requirements[i].AccessLevel < out.Requirements[j].AccessLevel
	})
	logger.Info("agent required identities resolved",
		"requirements", len(out.Requirements), "satisfied", out.Satisfied)
	requesthelper.WriteJSON(req, resp, http.StatusOK, out)
}

// wildcardToolNames expands the "*" requirement key into the tool names it
// covers, per server, so the answer names tools a user recognises instead of
// a piece of configuration syntax. Tools with requirements of their own are
// left out: the wildcard does not apply to them.
//
// The tool catalogue is only fetched when some server actually uses the
// wildcard, and a failure degrades to reporting "*" rather than failing the
// request — this is a display detail, not the answer itself.
func (a *MainAPI) wildcardToolNames(ctx context.Context, logger *logging.Logger, servers []tools.Server) map[string][]string {
	ids := []string{}
	for _, server := range servers {
		if _, wildcard := server.ToolAccessRequirements[tools.WildcardTool]; wildcard {
			ids = append(ids, server.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]string, len(ids))
	catalogue, err := a.toolRegistry.ListTools(ctx, ids)
	if err != nil {
		logger.Warn("failed to list tools for the wildcard requirements", "servers", ids, "err", err)
	}
	own := make(map[string]map[string][]tools.AccessRequirement, len(servers))
	for _, server := range servers {
		own[server.ID] = server.ToolAccessRequirements
	}
	for _, tool := range catalogue {
		// A tool with requirements of its own is not covered by the wildcard.
		// (A tool named like a probe key cannot exist: MCP tool names never
		// start with "/".)
		if _, specific := own[tool.ServerID][tool.Name]; specific {
			continue
		}
		out[tool.ServerID] = append(out[tool.ServerID], tool.Name)
	}
	// A server whose catalogue is empty or unreachable still has to say that
	// the credential is needed, so keep the literal key.
	for _, id := range ids {
		if len(out[id]) == 0 {
			out[id] = []string{tools.WildcardTool}
		}
	}
	return out
}

// appendUnique adds value unless the slice already has it. The slices are a
// handful of ids, so a linear scan beats keeping a set per entry.
func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// credentialStatus reports whether the session user already holds a usable
// credential for one requirement. It asks the same question the tool loop
// asks, so the two cannot disagree.
func (a *MainAPI) credentialStatus(ctx context.Context, logger *logging.Logger, r agentRequirement) (bool, string) {
	required := []string(nil)
	if r.AccessLevel != "" {
		required = []string{r.AccessLevel}
	}
	_, found, err := a.identities.Credentials(ctx, r.Provider, required)
	switch {
	case err == nil && found:
		return true, ""
	case err == nil:
		return false, "not_connected"
	}
	var conflict *extidentities.ConflictError
	if errors.As(err, &conflict) {
		return false, "insufficient_access"
	}
	logger.Warn("could not determine credential status", "provider", r.Provider, "err", err)
	return false, "unavailable"
}
