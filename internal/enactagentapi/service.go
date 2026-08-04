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
	"enact/internal/service"
)

// Config wires the runtime, OpenSearch, the Redis queue (RAG document
// hand-off to the indexer), and the KB service client used to validate the
// knowledge bases an agent references.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	Agents     agents.Config
	KBAPI      kb.ClientConfig
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
		kbClient := kb.NewClient(cfg.KBAPI)
		producer := queue.NewProducer(cfg.Queue)
		logger.Info("agent api initialized", "stream", cfg.Queue.Stream, "kb_api", cfg.KBAPI.BaseURL)
		return []*restful.WebService{newAgentAPI(agentRepo, rags, kbClient, producer, logger).WebService()}, nil
	}
}
