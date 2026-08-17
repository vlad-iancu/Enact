package enactmain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// mcpServersWebService is the browser-facing MCP server management: the
// registry owns the records; enact-main scopes them to the logged-in user
// via the owner field.
func (a *MainAPI) mcpServersWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/mcp-servers").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireSession)

	ws.Route(ws.POST("").
		To(a.createMCPServer).
		Consumes(restful.MIME_JSON).
		Reads(tools.CreateServerRequest{}).
		Doc("Register an MCP server owned by the logged-in user; the registry health-checks it and caches its tools").
		Returns(http.StatusCreated, "Registered", tools.Server{}).
		Returns(http.StatusBadRequest, "Invalid request or unreachable server", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.listMCPServers).
		Doc("List the logged-in user's MCP servers").
		Returns(http.StatusOK, "OK", mcpServersResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.GET("/tools").
		To(a.listMCPTools).
		Doc("List the cached tool definitions of the logged-in user's MCP servers").
		Returns(http.StatusOK, "OK", mcpToolsResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateMCPServer).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "server id")).
		Doc("Update one of the user's MCP servers (partial: absent fields keep their values)").
		Returns(http.StatusOK, "Updated", tools.Server{}).
		Returns(http.StatusBadRequest, "Invalid request or unreachable server", errorResponse{}).
		Returns(http.StatusNotFound, "No such server", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteMCPServer).
		Param(ws.PathParameter("id", "server id")).
		Doc("Delete one of the user's MCP servers and its cached tools").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "No such server", errorResponse{}).
		Returns(http.StatusUnauthorized, "No session", errorResponse{}))

	return ws
}

// mcpServerItem is a registered server plus what the caller may do with it;
// see resourceFlags. "Usable" means an agent of theirs may call its tools.
type mcpServerItem struct {
	tools.Server
	resourceFlags
}

type mcpServersResponse struct {
	Servers []mcpServerItem `json:"servers"`
}

type mcpToolsResponse struct {
	Tools []tools.Tool `json:"tools"`
}

func (a *MainAPI) createMCPServer(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create mcp server requested")

	var body tools.CreateServerRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	// Ownership is the session's, never the caller's claim.
	body.Owner = sess.UserID
	logger.Info("create decoded", "id", body.ID, "url", body.URL, "transport", body.TransportType)

	created, err := a.toolRegistry.Create(req.Request.Context(), body)
	if err != nil {
		logger.Warn("mcp server creation failed", "err", err)
		relayToolsErr(req, resp, err, "register MCP server")
		return
	}
	logger.Info("mcp server registered", "id", created.ID, "tool_count", created.ToolCount)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) listMCPServers(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list mcp servers requested")

	servers, err := a.toolRegistry.List(req.Request.Context(), nil, sess.UserID)
	if err != nil {
		logger.Error("failed to list mcp servers", "err", err)
		relayToolsErr(req, resp, err, "list MCP servers")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	out := make([]mcpServerItem, 0, len(servers))
	for _, server := range servers {
		out = append(out, mcpServerItem{
			Server:        server,
			resourceFlags: flagsFor(effective, rbac.ResourceMCPServer, server.ID),
		})
	}
	logger.Info("mcp servers listed", "count", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, mcpServersResponse{Servers: out})
}

func (a *MainAPI) listMCPTools(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list mcp tools requested")

	servers, err := a.toolRegistry.List(req.Request.Context(), nil, sess.UserID)
	if err != nil {
		logger.Error("failed to list mcp servers", "err", err)
		relayToolsErr(req, resp, err, "list MCP tools")
		return
	}
	if len(servers) == 0 {
		requesthelper.WriteJSON(req, resp, http.StatusOK, mcpToolsResponse{Tools: []tools.Tool{}})
		return
	}
	ids := make([]string, len(servers))
	for i, s := range servers {
		ids[i] = s.ID
	}
	organizationID, ok := a.conversationOrganization(req, resp)
	if !ok {
		return
	}
	defs, err := a.toolRegistry.ListTools(req.Request.Context(), organizationID, ids)
	if err != nil {
		logger.Error("failed to list mcp tools", "err", err)
		relayToolsErr(req, resp, err, "list MCP tools")
		return
	}
	logger.Info("mcp tools listed", "servers", len(ids), "tools", len(defs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, mcpToolsResponse{Tools: defs})
}

// ownedMCPServer loads a server and verifies the session owns it. A foreign
// server reads as 404 — its existence is not this user's business.
func (a *MainAPI) ownedMCPServer(req *restful.Request, resp *restful.Response, id string) (tools.Server, bool) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger)
	server, found, err := a.toolRegistry.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to load mcp server", "id", id, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to load MCP server")
		return tools.Server{}, false
	}
	if !found || server.Owner != sess.UserID {
		logger.Warn("mcp server not found or not owned", "id", id, "found", found)
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("MCP server %q not found", id))
		return tools.Server{}, false
	}
	return server, true
}

func (a *MainAPI) updateMCPServer(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("server_id", id)
	logger.Info("update mcp server requested")

	if _, ok := a.ownedMCPServer(req, resp, id); !ok {
		return
	}
	rawBody, err := io.ReadAll(req.Request.Body)
	if err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
		return
	}
	updated, found, err := a.toolRegistry.Update(req.Request.Context(), id, rawBody)
	if err != nil {
		logger.Warn("mcp server update failed", "err", err)
		relayToolsErr(req, resp, err, "update MCP server")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("MCP server %q not found", id))
		return
	}
	logger.Info("mcp server updated", "id", id)
	requesthelper.WriteJSON(req, resp, http.StatusOK, updated)
}

func (a *MainAPI) deleteMCPServer(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("server_id", id)
	logger.Info("delete mcp server requested")

	if _, ok := a.ownedMCPServer(req, resp, id); !ok {
		return
	}
	found, err := a.toolRegistry.Delete(req.Request.Context(), id)
	if err != nil {
		logger.Error("mcp server deletion failed", "err", err)
		relayToolsErr(req, resp, err, "delete MCP server")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("MCP server %q not found", id))
		return
	}
	logger.Info("mcp server deleted", "id", id)
	resp.WriteHeader(http.StatusNoContent)
}

// relayToolsErr maps registry-client errors onto user-facing responses:
// validation/conflict/unreachable-server messages pass through as 400,
// everything else is a 502.
func relayToolsErr(req *restful.Request, resp *restful.Response, err error, action string) {
	var forbidden *requesthelper.ForbiddenError
	if errors.As(err, &forbidden) {
		// A downstream refusal, relayed as one. Flattening it into the 502
		// below would tell a user their platform is broken when in fact they
		// need a role.
		requesthelper.WriteError(req, resp, http.StatusForbidden, forbidden.Message)
		return
	}
	var badReq *requesthelper.BadRequestError
	if errors.As(err, &badReq) {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, badReq.Message)
		return
	}
	requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to "+action)
}
