package enactmcpservers

import (
	"net/http"
	"sort"
	"strings"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"enact/internal/builtinmcp"
	"enact/internal/enactmcpservers/googleworkspace"
	"enact/internal/logging"
	"enact/internal/requesthelper"
)

// hostedServer is one built-in MCP server: the path segment it is mounted
// under, and a factory that builds it for one request's credential.
//
// The factory takes the token rather than the server being built once,
// because a tool acts as the caller and there is no other place to put that:
// the go-sdk hands the HTTP request to this factory precisely so per-request
// state can be bound here.
type hostedServer struct {
	definition builtinmcp.Server
	handler    http.Handler
}

// HostedAPI serves every built-in MCP server.
type HostedAPI struct {
	servers []*hostedServer
	logger  *logging.Logger
}

func newHostedAPI(logger *logging.Logger) *HostedAPI {
	a := &HostedAPI{logger: logger}
	// The catalogue is shared with enact-main, which publishes these paths to
	// the browser — so the path a server is mounted at and the URL a user is
	// offered cannot drift apart.
	for _, server := range builtinmcp.Servers() {
		switch server.ID {
		case "google-workspace":
			a.mount(server, googleworkspace.NewServer)
		default:
			logger.Warn("catalogue names a server this binary does not implement", "id", server.ID)
		}
	}
	return a
}

// mount registers one server and its streamable-http handler.
func (a *HostedAPI) mount(definition builtinmcp.Server, build func(token string) *mcp.Server) {
	hosted := &hostedServer{definition: definition}
	hosted.handler = mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return build(bearerToken(r)) },
		&mcp.StreamableHTTPOptions{
			// Stateless: one server, no session to keep. Every request
			// carries its own credential, so there is nothing worth
			// remembering between them — and a session would only invite a
			// second request to reuse the first one's token.
			Stateless: true,
			// Answer JSON rather than an event stream. These tools are
			// request/response; nothing streams, and JSON is easier for
			// anything reading the traffic.
			JSONResponse: true,
		})
	a.servers = append(a.servers, hosted)
}

// serverNames lists what is mounted, for the startup log.
func (a *HostedAPI) serverNames() []string {
	names := make([]string, 0, len(a.servers))
	for _, s := range a.servers {
		names = append(names, s.definition.ID)
	}
	sort.Strings(names)
	return names
}

// mountRoot is the WebService root every hosted server hangs under. It matches
// the prefix the catalogue gives their paths (internal/builtinmcp), so the
// served URL is unchanged.
const mountRoot = "/mcp-servers"

// WebService mounts every hosted server at the path the catalogue gives it.
//
// The MCP handler is a plain http.Handler, so the route delegates to it with
// the raw writer and request — the same shape the tool registry uses to proxy
// MCP traffic through go-restful.
func (a *HostedAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	// The root path MUST be set, and must not be "/".
	//
	// go-restful's Container.Add calls os.Exit(1) — not an error, not a panic
	// — when two WebServices share a root path, and the service runtime
	// already registers the home page at "/". A WebService with no Path
	// defaults to "/", so leaving it unset kills the process at startup with
	// nothing logged but go-restful's own one-line warning.
	ws.Path(mountRoot).Produces(restful.MIME_JSON)

	for _, hosted := range a.servers {
		server := hosted
		// Routes are relative to the root, which go-restful joins back onto
		// it — so the served path is still the catalogue's.
		routePath := strings.TrimPrefix(server.definition.Path, mountRoot)
		// GET and DELETE are registered even though a stateless handler
		// answers them with 405: routing them here produces the transport's
		// own answer rather than go-restful's 404, which is what an MCP
		// client is written to expect.
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			ws.Route(ws.Method(method).Path(routePath).
				To(func(req *restful.Request, resp *restful.Response) {
					a.serve(server, req, resp)
				}).
				Doc("MCP streamable-http endpoint for the built-in " + server.definition.Name + " server; " +
					"the caller's third-party credential is taken from the Authorization header").
				Consumes("*/*"))
		}
	}
	return ws
}

// serve hands the request to the MCP transport.
func (a *HostedAPI) serve(server *hostedServer, req *restful.Request, resp *restful.Response) {
	// The token is never logged — only whether one arrived (ADR-0008).
	requesthelper.Logger(req, a.logger).Info("mcp request",
		"server", server.definition.ID, "method", req.Request.Method,
		"credential_present", bearerToken(req.Request) != "")
	server.handler.ServeHTTP(resp.ResponseWriter, req.Request)
}

// bearerToken reads the caller's third-party credential.
//
// Empty is not an error here: it becomes a tool error naming what is missing,
// which reaches the model and the user, rather than a transport failure that
// reads like the server is broken.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return ""
}
