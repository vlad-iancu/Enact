// Package enactcrawlorchestrator schedules and executes focused crawls.
//
// It exposes no API. Two goroutines started by Build do the work: a ticker
// that queues crawls whose next run is due, and a consumer that executes
// queued runs. Both scheduled and manually-triggered runs arrive through the
// same queue, so there is exactly one execution path.
//
// The service reuses the standard runtime, so it still gets /healthz and
// graceful shutdown; the loops are tied to the process lifetime through the
// context Build is given.
package enactcrawlorchestrator

import (
	"context"
	"strconv"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/babelnet"
	"enact/internal/crawler"
	"enact/internal/crawls"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/ner"
	"enact/internal/opensearch"
	"enact/internal/queue"
	"enact/internal/s2s"
	"enact/internal/service"
	"enact/internal/wsd"
)

// Config wires everything a crawl needs: storage, the queue, the sense
// inventories, the fetcher's limits, and the KB service it writes into.
type Config struct {
	service.Config
	OpenSearch opensearch.Config
	Queue      queue.Config
	Crawls     crawls.Config
	KBAPI      kb.ClientConfig
	BabelNet   babelnet.Config
	WordNet    wsd.Config
	NER        ner.Config
	Crypto     crawls.CryptoConfig
	Fetch      crawler.FetchConfig
	S2S        s2s.Config

	// SweepInterval is how often the scheduler looks for due crawls. It is
	// the granularity of scheduling, not the crawl interval — a crawl due at
	// 03:00 with a one-minute sweep starts within a minute of it.
	SweepInterval time.Duration `env:"CRAWL_SWEEP_INTERVAL, default=1m"`
	// SweepBatch caps how many due crawls one sweep queues, so a backlog is
	// worked through steadily rather than all at once.
	SweepBatch int `env:"CRAWL_SWEEP_BATCH, default=20"`
	// SalientTerms is how many of a page's terms are disambiguated.
	SalientTerms int `env:"CRAWL_SALIENT_TERMS, default=40"`
	// EntityWeight is what a name in the query counts for lexically, against
	// 1 for an ordinary word. See wsd.QueryTerms.
	EntityWeight float64 `env:"CRAWL_ENTITY_WEIGHT, default=3"`
	// NameMissPenalty multiplies the score of a page that mentions none of the
	// query's names. 1 disables it. See crawler.DefaultNameMissPenalty.
	NameMissPenalty float64 `env:"CRAWL_NAME_MISS_PENALTY, default=0.5"`
	// RunTimeout bounds one run end to end, independently of the crawl's own
	// MaxDuration, so a wedged run cannot hold a consumer slot forever.
	RunTimeout time.Duration `env:"CRAWL_RUN_TIMEOUT, default=30m"`
	// DisableBabelNet takes BabelNet out of the picture entirely: the query is
	// disambiguated against the local WordNet, the same inventory the pages
	// use, and no request is ever made to babelnet.io.
	//
	// Worth having as a deliberate mode rather than only as a fallback. The
	// free tier is 1000 requests a day and refuses with a 403 that says
	// nothing about why, so a deployment can spend weeks discovering the
	// allowance is gone one paused run at a time. Turning BabelNet off makes
	// runs cheap, offline, reproducible and free of a third party — at the
	// cost of every encyclopaedic sense, which is a real cost: WordNet has no
	// entry for "OpenSearch", no database sense of "index" and no computing
	// sense of "security", and no algorithm can choose a sense it was never
	// offered.
	//
	// Default false, so setting nothing keeps the behaviour a deployment
	// already has.
	DisableBabelNet bool `env:"DISABLE_BABELNET, default=false"`
}

// Build wires the orchestrator and starts its two loops.
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

		// WordNet is parsed once and shared: it backs Wu-Palmer similarity,
		// lemmatisation, and every page's sense lookup. A few hundred
		// milliseconds and a little over a hundred megabytes, paid at startup
		// so no crawl pays it.
		taxonomy, err := wsd.NewTaxonomy(cfg.WordNet)
		if err != nil {
			logger.Error("failed to load wordnet", "err", err)
			return nil, err
		}
		// Pages always use WordNet. When BabelNet is disabled the query uses
		// the very same instance, which is what stops the fallback in
		// PrepareRelevance from firing: there is nothing to fall back TO, and
		// a run on the configured inventory is not a degraded run.
		pageInventory := wsd.NewWordNetInventory(taxonomy)
		queryInventory := wsd.Inventory(pageInventory)
		budget := "n/a"
		if !cfg.DisableBabelNet {
			inventory := babelnet.New(cfg.BabelNet, osClient, logger)
			if err := inventory.EnsureIndex(ctx); err != nil {
				logger.Error("failed to verify the babelnet cache index", "err", err)
				return nil, err
			}
			queryInventory = inventory
			budget = strconv.Itoa(inventory.Remaining())
		}

		// A name recogniser, if one is configured. Its absence is not an
		// error: this is the tree's only native dependency, and a deployment
		// that cannot satisfy it must still crawl. A model that fails to load
		// is logged and skipped rather than fatal, because a crawl with
		// slightly worse name detection beats a service that will not start.
		var recognizer crawler.NameRecognizer
		if model, err := ner.New(cfg.NER); err != nil {
			logger.Error("name recognition is enabled but unavailable; continuing without it",
				"err", err, "model_dir", cfg.NER.ModelDir)
		} else if model != nil {
			recognizer = model
			logger.Info("name recognition loaded", "model_dir", cfg.NER.ModelDir)
		}

		vault, err := crawls.NewVault(cfg.Crypto)
		if err != nil {
			logger.Error("failed to load the crawl credential key (CRAWL_ENCRYPTION_KEY)", "err", err)
			return nil, err
		}

		s2sRuntime, err := s2s.Load(cfg.S2S, logger)
		if err != nil {
			logger.Error("failed to load s2s configuration", "err", err)
			return nil, err
		}
		kbClient := kb.NewClient(cfg.KBAPI, s2sRuntime.Transport(nil, "enact-kb-api"))
		producer := queue.NewProducer(cfg.Queue)

		fetcher := crawler.NewFetcher(cfg.Fetch)
		runner := &Runner{
			crawls: repo, runs: runs, kbs: kbClient,
			taxonomy: taxonomy, queryInventory: queryInventory,
			pageInventory:         pageInventory,
			crawler:               crawler.New(fetcher, logger),
			fetcher:               fetcher,
			allowPrivateAddresses: cfg.Fetch.AllowPrivateAddresses,
			recognizer:            recognizer,
			salientTerms:          cfg.SalientTerms, entityWeight: cfg.EntityWeight,
			nameMissPenalty: cfg.NameMissPenalty,
			vault:           vault,
			runTimeout:      cfg.RunTimeout,
			logger:          logger,
		}
		scheduler := &Scheduler{
			crawls: repo, runs: runs, producer: producer,
			batch: cfg.SweepBatch, logger: logger,
		}

		logger.Info("crawl orchestrator initialized",
			"stream", cfg.Queue.Stream, "group", cfg.Queue.Group,
			"sweep_interval", cfg.SweepInterval, "wordnet_synsets", taxonomy.Size(),
			"babelnet_disabled", cfg.DisableBabelNet,
			"ner_enabled", recognizer != nil,
			"babelnet_budget_remaining", budget,
			"kb_api", cfg.KBAPI.BaseURL, "s2s_key_id", cfg.S2S.KeyID)

		go scheduler.Loop(ctx, cfg.SweepInterval)

		consumer := queue.NewConsumer(cfg.Queue)
		consumer.Dropped = func(id string, deliveries int64) {
			logger.Warn("dropping poison crawl run message after max delivery attempts",
				"message_id", id, "deliveries", deliveries, "stream", cfg.Queue.Stream)
		}
		go func() {
			logger.Info("consuming crawl runs", "stream", cfg.Queue.Stream, "group", cfg.Queue.Group)
			if err := consumer.RunCrawls(ctx, runner.Handle); err != nil {
				logger.Error("consumer stopped", "stream", cfg.Queue.Stream, "err", err)
			}
		}()

		// No web services: this is a worker. The runtime still gives it
		// /healthz and a clean shutdown.
		return []*restful.WebService{}, nil
	}
}
