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
	"enact/internal/queue"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/tools"
)

// Config wires the runtime, OpenSearch, the Redis queue (RAG document
// hand-off to the indexer), and the KB service client used to validate the
// knowledge bases an agent references.
type Config struct {
	service.Config
	OpenSearch   opensearch.Config
	Queue        queue.Config
	Agents       agents.Config
	KBAPI        kb.ClientConfig
	ToolRegistry tools.ClientConfig
	S2S          s2s.Config
}

// Build constructs the agent management API, verifying the backing indices
// exist (they are created by `make infrastructure-up`). Knowledge-base
// references are validated against the enact-kb-api service over HTTP; the
// RAG repository backs the per-agent RAG collections.
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
		rags := agents.NewRAGRepository(osClient, cfg.Agents)
		if err := rags.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify agent rag chunk index", "err", err)
			return nil, err
		}
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		kbClient := kb.NewClient(cfg.KBAPI, s2sRuntime.Transport(nil, "enact-kb-api"))
		toolsClient := tools.NewClient(cfg.ToolRegistry, s2sRuntime.Transport(nil, "enact-tool-registry"))
		producer := queue.NewProducer(cfg.Queue)
		logger.Info("agent api initialized", "stream", cfg.Queue.Stream, "kb_api", cfg.KBAPI.BaseURL, "tool_registry", cfg.ToolRegistry.BaseURL, "s2s_key_id", cfg.S2S.KeyID)
		ws := newAgentAPI(agentRepo, rags, kbClient, toolsClient, producer, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
