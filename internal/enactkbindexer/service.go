// Package enactkbindexer implements the document-processing worker. It
// consumes document messages from the Redis Streams queue and, after Tika
// text extraction, either stores KB context documents whole or chunks and
// embeds agent RAG documents into the k-NN index.
//
// The worker reuses the standard service runtime so it gets a /healthz probe
// and graceful shutdown; the consumer loop runs in a background goroutine
// started by Build and tied to the process lifetime.
package enactkbindexer

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/agents"
	"enact/internal/bedrock"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/rag"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/tika"
)

// Config wires the runtime, OpenSearch, Redis queue, and Bedrock embeddings.
type Config struct {
	service.Config
	OpenSearch     opensearch.Config
	Queue          queue.Config
	Bedrock        bedrock.ClientConfig
	Tika           tika.Config
	KB             kb.Config
	Agents         agents.Config
	S2S            s2s.Config
	EmbeddingModel string `env:"BEDROCK_EMBEDDING_MODEL, default=amazon.titan-embed-text-v2:0"`
	ChunkSize      int    `env:"RAG_CHUNK_SIZE, default=1000"`
	ChunkOverlap   int    `env:"RAG_CHUNK_OVERLAP, default=150"`
}

// Build sets up the worker dependencies, ensures indices exist, and launches
// the consumer loop. It registers no HTTP routes of its own (health/home/
// swagger come from the service runtime).
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		// The indexer exposes no business routes and calls no enact service,
		// but loading validates its key material so a fleet-wide rotation
		// cannot silently leave it misconfigured.
		if _, err := s2s.Load(cfg.S2S, logger); err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		embedder, err := bedrock.NewClient(ctx, cfg.Bedrock)
		if err != nil {
			logger.Error("failed to create bedrock client", "err", err)
			return nil, err
		}
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		documents := kb.NewDocumentRepository(osClient, cfg.KB)
		if err := documents.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify kb documents index", "err", err)
			return nil, err
		}
		// Chunks belong to knowledge bases now, so the repository comes from
		// the kb domain and is scoped by kb id.
		chunks := kb.NewChunkRepository(osClient, cfg.KB)
		if err := chunks.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify the knowledge-base chunk index", "err", err)
			return nil, err
		}

		chunkSize := cfg.ChunkSize
		if chunkSize <= 0 {
			chunkSize = rag.DefaultChunkSize
		}
		w := &worker{
			documents:    documents,
			chunks:       chunks,
			extractor:    tika.NewClient(cfg.Tika),
			embedder:     embedder,
			embedModel:   cfg.EmbeddingModel,
			chunkSize:    chunkSize,
			chunkOverlap: cfg.ChunkOverlap,
			logger:       logger,
		}

		consumer := queue.NewConsumer(cfg.Queue)
		consumer.Dropped = func(id string, deliveries int64) {
			logger.Warn("dropping poison message after max delivery attempts",
				"message_id", id, "deliveries", deliveries, "stream", cfg.Queue.Stream)
		}
		go func() {
			logger.Info("consuming document stream", "stream", cfg.Queue.Stream, "group", cfg.Queue.Group)
			if err := consumer.Run(ctx, w.handle); err != nil {
				logger.Error("consumer stopped", "stream", cfg.Queue.Stream, "err", err)
			}
		}()

		return []*restful.WebService{}, nil
	}
}
