// Package enactkbapi implements the knowledge-base API. Users create and
// delete knowledge bases (metadata stored in OpenSearch) and upload documents
// to them. Uploaded documents are NOT indexed inline; instead they are pushed
// onto the Redis Streams work queue for the enact-kb-document-indexer to
// process asynchronously.
package enactkbapi

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config wires the service runtime, OpenSearch (metadata), and the Redis
// queue (document hand-off).
//
// service.Config is embedded (service.Run locates it by type); the dependency
// configs are named fields so their identically-named Config types do not
// collide. go-envconfig recurses into named struct fields, so their env tags
// are still honoured.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	KB         kb.Config
	RBAC       rbac.ClientConfig
	S2S        s2s.Config
}

// Build constructs the KB API, verifying the backing indices exist (they are
// created by `make infrastructure-up`). Both knowledge-base domain
// repositories are used: KB metadata for the CRUD and documents so deleting
// a KB can cascade to the documents stored under it.
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		kbs := kb.NewRepository(osClient, cfg.KB)
		if err := kbs.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify knowledge-base index", "err", err)
			return nil, err
		}
		documents := kb.NewDocumentRepository(osClient, cfg.KB)
		if err := documents.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify kb documents index", "err", err)
			return nil, err
		}
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		producer := queue.NewProducer(cfg.Queue)
		logger.Info("kb api initialized", "stream", cfg.Queue.Stream, "s2s_key_id", cfg.S2S.KeyID)
		// This service checks its own permissions: it is reachable by any
		// signed service caller, not only through enact-main.
		rbacClient := rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		enforcer := rbac.NewEnforcer(rbacClient, cfg.RBAC)
		ws := newKBAPI(kbs, documents, producer, rbacClient, enforcer, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
