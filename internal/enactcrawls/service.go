// Package enactcrawls implements the focused-crawl API: authoring crawl
// definitions and asking for runs.
//
// It does no crawling. Fetching pages takes minutes and reaches third-party
// sites, so a run is written to OpenSearch, queued, and executed by
// enact-crawl-orchestrator — the same split as workflows, and for the same
// reason: an HTTP handler that waited would tie up a connection for the life
// of a crawl and lose the run on a disconnect.
package enactcrawls

import (
	"context"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/crawls"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/s2s"
	"enact/internal/service"
)

// Config wires the runtime, OpenSearch, the run queue, the KB service client
// (used to check that a crawl's target knowledge base is a suitable one) and
// RBAC.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	Crawls     crawls.Config
	KBAPI      kb.ClientConfig
	RBAC       rbac.ClientConfig
	S2S        s2s.Config
	// Crypto seals the credential headers a crawl presents to authenticated
	// sites. Its own key rather than the identity service's: a crawl header
	// and an OAuth refresh token are different secrets with different blast
	// radii, and one leaked key should not open both.
	Crypto crawls.CryptoConfig
}

// Build constructs the crawl API, verifying its indices exist (they are
// created by `make infrastructure-up`).
func Build(cfg *Config) service.Builder {
	return func(ctx context.Context) ([]*restful.WebService, error) {
		logger := logging.New().WithFields("service", cfg.Name)
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			logger.Error("failed to create opensearch client", "err", err)
			return nil, err
		}
		repo := crawls.NewRepository(osClient, cfg.Crawls)
		if err := repo.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify crawl index", "err", err)
			return nil, err
		}
		runs := crawls.NewRunRepository(osClient, cfg.Crawls)
		if err := runs.EnsureIndex(ctx); err != nil {
			logger.Error("failed to verify crawl run index", "err", err)
			return nil, err
		}
		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		kbClient := kb.NewClient(cfg.KBAPI, s2sRuntime.Transport(nil, "enact-kb-api"))
		// Authorization is this service's own job: it is reachable by any
		// signed service caller, so a check that lives only in enact-main is
		// a check a second caller walks around.
		rbacClient := rbac.NewClient(cfg.RBAC, s2sRuntime.Transport(nil, "enact-rbac"))
		enforcer := rbac.NewEnforcer(rbacClient, cfg.RBAC)
		producer := queue.NewProducer(cfg.Queue)

		logger.Info("crawl api initialized", "stream", cfg.Queue.Stream,
			"kb_api", cfg.KBAPI.BaseURL, "rbac", cfg.RBAC.BaseURL, "s2s_key_id", cfg.S2S.KeyID)
		vault, err := crawls.NewVault(cfg.Crypto)
		if err != nil {
			logger.Error("failed to load the crawl credential key (CRAWL_ENCRYPTION_KEY)", "err", err)
			return nil, err
		}
		ws := newCrawlAPI(repo, runs, kbClient, producer, rbacClient, enforcer, vault, logger).WebService()
		if s2sRuntime.Enabled() {
			ws.Filter(s2sRuntime.Filter)
		}
		return []*restful.WebService{ws}, nil
	}
}
