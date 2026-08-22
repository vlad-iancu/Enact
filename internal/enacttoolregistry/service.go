// Package enacttoolregistry implements the MCP tool registry service: the
// source of truth for registered MCP servers (enact-tool-servers), a cache
// of the tools they advertise (enact-tool-cache, refreshed on a fixed
// interval), and an MCP-compliant proxy so MCP clients can reach any
// registered server through the platform.
package enacttoolregistry

import (
	"context"
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/extidentities"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/tools"
)

// Config wires the runtime, OpenSearch, S2S, and the cache refresh cadence.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Tools      tools.Config
	S2S        s2s.Config
	// Identities resolves the credentials a server's owner configured for
	// the platform's own probes (tools.ProbeInitialize / ProbeListTools).
	Identities extidentities.ClientConfig
	RBAC       rbac.ClientConfig

	// RefreshAt is the interval on which every registered server's tool
	// cache is re-fetched.
	RefreshAt time.Duration `env:"REFRESH_AT, default=5m"`
	// MCPTimeout bounds one connect+list against an underlying MCP server
	// (registration health checks and cache refreshes).
	//
	// 15s was too tight to be a health check. Connecting opens the standalone
	// SSE stream the spec defines, and a real server can sit on it: Atlassian
	// sends its first byte after 15.2 seconds, so a probe died a fifth of a
	// second short and blamed whichever message came next. The budget covers
	// the handshake, not the server's throughput, so it can afford to be
	// generous — tools.ClientConfig.Timeout must stay above it.
	MCPTimeout time.Duration `env:"MCP_CONNECT_TIMEOUT, default=45s"`
	// BuiltInMCPURL is where enact-mcp-servers actually listens. Servers are
	// registered under the portless alias builtinmcp.AliasHost, and this is
	// what that alias resolves to when the registry dials — so the address
	// lives in the deployment rather than in every user's record.
	BuiltInMCPURL string `env:"MCP_SERVERS_URL, default=http://localhost:8010"`
}

// Build constructs the registry service and starts the refresh loop.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		repo := tools.NewRepository(osClient, cfg.Tools)
		if err := repo.EnsureIndices(ctx); err != nil {
			logger.Error("failed to verify tool indices", "err", err)
			return nil, err
		}

		rbacClient := rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		api := &RegistryAPI{
			repo:       repo,
			mcpTimeout: cfg.MCPTimeout,
			// Proxied MCP exchanges stream (SSE) and outlive any sane
			// client timeout; the per-request context bounds them instead.
			proxyClient: &http.Client{},
			identities:  extidentities.NewClient(cfg.Identities, s2sRuntime.Transport(nil, "enact-external-identities")),
			rbac:        rbacClient,
			enforcer:    rbac.NewEnforcer(rbacClient, cfg.RBAC),
			// Probes talk to third-party MCP servers directly, so this client
			// carries no S2S signing — only whatever the owner's credential
			// configuration renders.
			//
			// No client-level Timeout, for the same reason proxyClient has
			// none: a server that answers initialize with text/event-stream
			// makes the SDK open a standalone SSE stream, and http.Client's
			// Timeout covers a whole request INCLUDING a streaming body. It
			// killed that stream, the SDK retried it five times, and a probe
			// that should take a second failed after ninety. probeTools
			// bounds the operation with a context instead.
			probeClient:   &http.Client{},
			builtInMCPURL: cfg.BuiltInMCPURL,
			logger:        logger,
		}
		logger.Info("tool registry initialized",
			"built_in_mcp_url", cfg.BuiltInMCPURL,
			"refresh_at", cfg.RefreshAt,
			"mcp_timeout", cfg.MCPTimeout,
			"external_identities", cfg.Identities.BaseURL,
			"servers_index", cfg.Tools.ServersIndex,
			"cache_index", cfg.Tools.CacheIndex,
			"s2s_key_id", cfg.S2S.KeyID,
		)

		go api.refreshLoop(ctx, cfg.RefreshAt)

		services := api.WebServices()
		if s2sRuntime.Enabled() {
			for _, ws := range services {
				ws.Filter(s2sRuntime.Filter)
			}
		}
		return services, nil
	}
}
