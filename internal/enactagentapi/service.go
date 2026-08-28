// Package enactagentapi implements CRUD for agents. An agent bundles a model
// (by friendly name), a system prompt, and the knowledge bases it retrieves
// from. Records are stored in OpenSearch.
package enactagentapi

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/tools"
)

// Config wires the runtime, OpenSearch, and the KB service client used to
// validate the knowledge bases an agent references.
type Config struct {
	service.Config
	OpenSearch   opensearch.Config
	Agents       agents.Config
	KBAPI        kb.ClientConfig
	ToolRegistry tools.ClientConfig
	RBAC         rbac.ClientConfig
	S2S          s2s.Config
}

// Build constructs the agent management API, verifying the agent index exists
// (it is created by `make infrastructure-up`). Knowledge-base references are
// validated against the enact-kb-api service over HTTP.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		agentRepo := agents.NewRepository(osClient, cfg.Agents)
		if err := agentRepo.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify agent index", "err", err)
			return nil, err
		}
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		kbClient := kb.NewClient(cfg.KBAPI, s2sRuntime.Transport(nil, "enact-kb-api"))
		toolsClient := tools.NewClient(cfg.ToolRegistry, s2sRuntime.Transport(nil, "enact-tool-registry"))
		// Authorization is this service's own job: it is reachable by any
		// signed service caller, so a check that lives only in enact-main is
		// a check a second caller walks around.
		rbacClient := rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		enforcer := rbac.NewEnforcer(rbacClient, cfg.RBAC)
		logger.Info("agent api initialized", "kb_api", cfg.KBAPI.BaseURL,
			"tool_registry", cfg.ToolRegistry.BaseURL, "rbac", cfg.RBAC.BaseURL, "s2s_key_id", cfg.S2S.KeyID)
		ws := newAgentAPI(agentRepo, kbClient, toolsClient, rbacClient, enforcer, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
