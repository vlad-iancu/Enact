package enacttoolregistry

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"enact/internal/identity"
	"enact/internal/mcp"
	"enact/internal/tools"
)

// probeTools opens a session against an MCP server and lists what it offers.
//
// A server may authenticate its handshake — GitHub's answers 401 before the
// JSON-RPC body is parsed — so the owner declares what to present under the
// tools.ProbeInitialize / ProbeListTools keys, and this resolves those
// credentials as the OWNER. That is the only identity available on the
// background refresh, and it is the right one everywhere: the probe is the
// platform's own connection to a server someone registered, not a user
// acting through it.
func (a *RegistryAPI) probeTools(ctx context.Context, server tools.Server) ([]mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, a.mcpTimeout)
	defer cancel()

	transport := &probeTransport{base: a.probeClient.Transport}
	initialize, err := a.probeHeaders(ctx, server, tools.ProbeInitialize)
	if err != nil {
		return nil, err
	}
	transport.use(initialize)

	// Always wrapped: with nothing configured the transport is a
	// pass-through, so there is one code path rather than two.
	client := &http.Client{Transport: transport, Timeout: a.probeClient.Timeout}
	// dialURL, not server.URL: a built-in server is registered under an
	// alias that resolves to nothing here.
	session, err := mcp.Connect(ctx, a.dialURL(server.URL), server.TransportType, client)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	// The two phases may present different credentials. Swapping the value
	// between them is safe because they are strictly sequential — and it
	// beats inspecting every request body to tell them apart.
	listing, err := a.probeHeaders(ctx, server, tools.ProbeListTools)
	if err != nil {
		return nil, err
	}
	transport.use(listing)
	return session.ListTools(ctx)
}

// probeHeaders resolves and renders the owner's credentials for one probe
// phase. No configuration means no headers, which is the ordinary case.
func (a *RegistryAPI) probeHeaders(ctx context.Context, server tools.Server, phase string) (map[string]string, error) {
	reqs, auth, configured := server.Probe(phase)
	if !configured {
		return nil, nil
	}
	logger := a.logger.WithFields("server_id", server.ID, "phase", phase, "owner", server.Owner)
	// As the owner, explicitly: on the refresh sweep there is no request
	// context to inherit an identity from.
	ownerCtx := identity.WithUserID(ctx, server.Owner)
	creds := make(tools.Credentials, len(reqs))
	for _, req := range reqs {
		cred, found, err := a.identities.Credentials(ownerCtx, req.Provider, req.RequiredAccess())
		switch {
		case err != nil:
			logger.Warn("failed to resolve the owner's probe credential", "provider", req.Provider, "err", err)
			return nil, fmt.Errorf("the server's owner has no usable %q credential: %w", req.Provider, err)
		case !found:
			logger.Warn("owner has not connected a provider the probe needs", "provider", req.Provider)
			at := ""
			if req.AccessLevel != "" {
				at = fmt.Sprintf(" at access level %q", req.AccessLevel)
			}
			return nil, fmt.Errorf("the server's owner has not connected %q%s, which this server needs to %s",
				req.Provider, at, phase)
		}
		creds[req.Provider] = cred
	}
	headers, params, err := tools.Render(auth, creds)
	if err != nil {
		return nil, fmt.Errorf("%s authorization could not be rendered: %w", phase, err)
	}
	if len(params) > 0 {
		// Params are tool-call arguments; there is no call here to put them
		// on. Say so rather than dropping them silently.
		logger.Warn("probe authorization declares params, which a handshake has nowhere to carry",
			"params", len(params))
	}
	logger.Info("probe credentials resolved", "providers", len(creds), "headers", tools.HeaderNames(headers))
	return headers, nil
}

// probeTransport stamps the current phase's headers on every request of a
// probe session. The value is swapped between phases, never mutated.
type probeTransport struct {
	base    http.RoundTripper
	headers atomic.Pointer[map[string]string]
}

func (t *probeTransport) use(headers map[string]string) {
	t.headers.Store(&headers)
}

func (t *probeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	headers := t.headers.Load()
	if headers == nil || len(*headers) == 0 {
		return base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	for name, value := range *headers {
		r.Header.Set(name, value)
	}
	return base.RoundTrip(r)
}
