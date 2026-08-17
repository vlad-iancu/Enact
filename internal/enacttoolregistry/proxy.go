package enacttoolregistry

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/tools"
)

// hopByHopHeaders are stripped in both directions per RFC 9110 §7.6.1.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// proxyMCP forwards a streamable-http exchange to the registered server's
// endpoint verbatim. GET (the standalone SSE channel), POST (JSON-RPC
// messages, possibly answered as an SSE stream), and DELETE (session
// teardown) all pass through.
func (a *RegistryAPI) proxyMCP(req *restful.Request, resp *restful.Response) {
	server, ok := a.proxyTarget(req, resp)
	if !ok {
		return
	}
	a.forward(req, resp, server, server.URL, false)
}

// proxySSE forwards the legacy SSE transport. The GET event stream carries
// an `endpoint` event naming the session's message URL on the underlying
// server; it is rewritten to point back through this proxy so a
// spec-compliant client keeps talking to the platform.
func (a *RegistryAPI) proxySSE(req *restful.Request, resp *restful.Response) {
	server, ok := a.proxyTarget(req, resp)
	if !ok {
		return
	}
	a.forward(req, resp, server, server.URL, true)
}

// proxySubpath forwards transport session paths (e.g. the message endpoint
// an SSE server advertised) to the same path on the underlying server's
// origin.
func (a *RegistryAPI) proxySubpath(req *restful.Request, resp *restful.Response) {
	server, ok := a.proxyTarget(req, resp)
	if !ok {
		return
	}
	base, err := url.Parse(server.URL)
	if err != nil {
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "invalid stored server url")
		return
	}
	target := base.Scheme + "://" + base.Host + "/" + req.PathParameter("subpath")
	a.forward(req, resp, server, target, false)
}

// proxyTarget resolves the {server-id} path parameter to its record.
func (a *RegistryAPI) proxyTarget(req *restful.Request, resp *restful.Response) (tools.Server, bool) {
	id := req.PathParameter("server-id")
	// Resolved within the caller's organization, so a server of another
	// organization is not proxied — it is simply not found.
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDenied(req, resp, err, "server not found")
		return tools.Server{}, false
	}
	server, found, err := a.repo.GetServer(req.Request.Context(), organizationID, id)
	if err != nil {
		requesthelper.Logger(req, a.logger).Error("failed to load server for proxy", "id", id, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to resolve server")
		return tools.Server{}, false
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("server %q not found", id))
		return tools.Server{}, false
	}
	return server, true
}

// forward streams one HTTP exchange to targetURL. rewriteEndpoint enables
// SSE endpoint-event rewriting (legacy SSE transport only).
func (a *RegistryAPI) forward(req *restful.Request, resp *restful.Response, server tools.Server, targetURL string, rewriteEndpoint bool) {
	logger := requesthelper.Logger(req, a.logger).WithFields("server_id", server.ID)
	r := req.Request
	logger.Info("proxying mcp exchange", "method", r.Method, "target", targetURL)

	target := targetURL
	if r.URL.RawQuery != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		logger.Error("failed to build proxy request", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to build proxy request")
		return
	}
	copyHeaders(out.Header, r.Header)
	applyToolAuth(out, logger)

	upstream, err := a.proxyClient.Do(out)
	if err != nil {
		logger.Warn("mcp server unreachable", "target", targetURL, "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadGateway, fmt.Sprintf("MCP server is not reachable: %v", err))
		return
	}
	defer func() { _ = upstream.Body.Close() }()

	w := resp.ResponseWriter
	copyHeaders(w.Header(), upstream.Header)
	w.WriteHeader(upstream.StatusCode)
	flusher, _ := w.(http.Flusher)

	if strings.HasPrefix(upstream.Header.Get("Content-Type"), "text/event-stream") {
		a.relaySSE(w, flusher, upstream, server, rewriteEndpoint, logger)
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := upstream.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// relaySSE forwards an event stream line by line, flushing per line so
// events reach the client as they happen. When rewriteEndpoint is set, the
// data of `endpoint` events is rebased onto this proxy's path space.
func (a *RegistryAPI) relaySSE(w http.ResponseWriter, flusher http.Flusher, upstream *http.Response, server tools.Server, rewriteEndpoint bool, logger *logging.Logger) {
	scanner := bufio.NewScanner(upstream.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inEndpointEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if rewriteEndpoint {
			if strings.HasPrefix(line, "event:") {
				inEndpointEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "endpoint"
			} else if inEndpointEvent && strings.HasPrefix(line, "data:") {
				original := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if rewritten, err := a.rewriteEndpointURL(server, original); err == nil {
					logger.Info("rewrote sse endpoint event", "from", original, "to", rewritten)
					line = "data: " + rewritten
				} else {
					logger.Warn("failed to rewrite sse endpoint event", "data", original, "err", err)
				}
			}
		}
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return
		}
		if line == "" && flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("sse relay ended with error", "err", err)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// rewriteEndpointURL maps a message-endpoint URI advertised by the
// underlying server onto this proxy: the client will hit
// /v1/servers/{id}/<path>?<query>, which proxySubpath forwards back to the
// server's origin.
func (a *RegistryAPI) rewriteEndpointURL(server tools.Server, raw string) (string, error) {
	base, err := url.Parse(server.URL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(ref)
	rewritten := "/v1/servers/" + url.PathEscape(server.ID) + resolved.Path
	if resolved.RawQuery != "" {
		rewritten += "?" + resolved.RawQuery
	}
	return rewritten, nil
}

// platformHeaders are the platform's own credentials and metadata. They
// stop at this boundary: everything past it is a third party, and
// forwarding them would hand a stranger a service token and a user id.
var platformHeaders = []string{"Authorization", identity.Header}

// applyToolAuth turns the tool-auth envelope into real upstream headers.
//
// Order matters: platform headers are removed FIRST, so a tool whose
// configured header is Authorization replaces the platform's S2S token
// rather than being overwritten by it.
func applyToolAuth(out *http.Request, logger *logging.Logger) {
	encoded := out.Header.Get(tools.HeaderToolAuth)
	out.Header.Del(tools.HeaderToolAuth)
	for _, name := range platformHeaders {
		out.Header.Del(name)
	}
	if encoded == "" {
		return
	}
	// Untrusted input: the proxy routes admit anonymous callers.
	headers, err := tools.DecodeAuthHeaders(encoded)
	if err != nil {
		logger.Warn("ignoring malformed tool auth envelope", "err", err, "envelope_bytes", len(encoded))
		return
	}
	for name, value := range headers {
		out.Header.Set(name, value)
	}
	// Names and sizes only, never values (ADR-0008).
	logger.Info("tool auth headers applied", "headers", tools.HeaderNames(headers), "envelope_bytes", len(encoded))
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isHopByHop(header string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(h, header) {
			return true
		}
	}
	return false
}
