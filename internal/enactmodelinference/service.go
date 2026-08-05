// Package enactmodelinference exposes the Bedrock-backed model inference
// service.
//
// Typical wiring from a cmd's main():
//
//	var cfg enactmodelinference.Config
//	service.Run(ctx, &cfg, enactmodelinference.Build(&cfg))
package enactmodelinference

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/bedrock"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config combines the generic service runtime settings with the Bedrock
// connection settings and the OpenSearch/embedding settings required for the
// agent (RAG) flow. service.Run loads the whole thing in one pass; the
// resulting struct is shared with the Builder via closure capture.
//
// OpenSearch and embedding settings are only exercised when a request carries
// an agent_id; plain (model-only) inference works without a reachable
// OpenSearch cluster.
type Config struct {
	service.Config
	bedrock.ClientConfig
	OpenSearch     opensearch.Config
	Agents         agents.Config
	AgentAPI       agents.ClientConfig
	KBAPI          kb.ClientConfig
	S2S            s2s.Config
	EmbeddingModel string `env:"BEDROCK_EMBEDDING_MODEL, default=amazon.titan-embed-text-v2:0"`
}

// Build returns a service.Builder that constructs the inference web service
// from the already-populated Config. Callers should declare the Config first,
// pass it to service.Run (which populates it from env), and pass the same
// pointer here.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		client, err := bedrock.NewClient(ctx, cfg.ClientConfig)
		if err != nil {
			logger.Error("failed to create bedrock client", "err", err)
			return nil, err
		}
		// Constructing the OpenSearch client does not open a connection, so
		// this is safe even when agents/RAG are not in use.
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		rags := agents.NewRAGRepository(osClient, cfg.Agents)
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		// Agent records and KB context documents come from their owning
		// services over HTTP; this service reads no agent or KB indices
		// directly. Only the RAG chunk index (vector retrieval) is local.
		agentClient := agents.NewClient(cfg.AgentAPI, s2sRuntime.Transport(nil, "enact-agent-management-api"))
		kbClient := kb.NewClient(cfg.KBAPI, s2sRuntime.Transport(nil, "enact-kb-api"))
		logger.Info("inference api initialized",
			"embedding_model", cfg.EmbeddingModel,
			"agent_api", cfg.AgentAPI.BaseURL,
			"kb_api", cfg.KBAPI.BaseURL,
			"s2s_key_id", cfg.S2S.KeyID,
		)
		api := newInferenceAPI(client, agentClient, rags, kbClient, cfg.EmbeddingModel, logger)
		ws := api.WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
