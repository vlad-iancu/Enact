// Package enactmcpservers hosts the MCP servers the platform ships itself,
// as opposed to the third-party ones organizations register by URL.
//
// It exists because some vendors' hosted MCP servers are not usable: Google's
// (`gmailmcp.googleapis.com`) answers `initialize` and `tools/list` to any
// valid OAuth token and then refuses every `tools/call` with "The caller does
// not have permission", while the same token reads the same mailbox through
// the REST API. Rather than run users' own code to work around that, the
// platform implements the tools against the public APIs.
//
// The servers here hold NO credentials. Each request carries the caller's
// bearer token, injected by the tool registry from the credential enact
// already stores, and that token is the only authority a tool has. This
// service can do nothing a caller could not do by calling Google directly.
package enactmcpservers

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config is the service configuration. There is no provider, database or
// credential configuration on purpose: everything a tool needs arrives on the
// request.
type Config struct {
	service.Config
	S2S s2s.Config
}

// Build returns the service.Builder for the built-in MCP servers.
func Build(cfg *Config) service.Builder {
	return func(_ context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}

		api := newHostedAPI(logger)
		logger.Info("mcp servers initialized",
			"servers", api.serverNames(), "s2s_key_id", cfg.S2S.KeyID)

		// The MCP web service is deliberately NOT wrapped in the S2S filter.
		//
		// S2S signs its JWT into the Authorization header, and that header
		// already carries the caller's Google token here — the reason the
		// tool-auth envelope exists at all (the registry translates the
		// envelope onto the upstream request as real headers). Two different
		// credentials cannot occupy one header.
		//
		// Nothing is lost by it: the Google token IS the authentication.
		// A caller without one gets nothing, and a caller with one could have
		// called Google directly. That is exactly the posture of every
		// third-party MCP server the platform talks to.
		_ = s2sRuntime
		return []*restful.WebService{api.WebService()}, nil
	}
}
