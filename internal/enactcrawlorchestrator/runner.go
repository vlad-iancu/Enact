package enactcrawlorchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"enact/internal/crawler"
	"enact/internal/crawls"
	"enact/internal/identity"
	"enact/internal/jira"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/requesthelper"
	"enact/internal/source"
	"enact/internal/wsd"
)

// A run REPLACES its knowledge base rather than reconciling with it: every
// document the crawl put there is deleted and the pages this run found are
// uploaded in their place. The knowledge base therefore always holds exactly
// what the most recent run saw, and nothing else.
//
// Two consequences follow, and both are deliberate:
//
//   - Every page is re-uploaded and re-embedded on every run, including the
//     ones whose content did not change. Content hashes are still recorded,
//     but they no longer save any work.
//   - Runs no longer resume from a previous run's frontier. Resuming would
//     store only the pages reached from that frontier, so each run would
//     wipe the previous corpus and replace it with a later, smaller slice of
//     the site. Every run starts from the seeds instead, which makes a run's
//     reach a function of max_pages rather than of how many times it has
//     run.

// Runner executes queued crawl runs.
type Runner struct {
	crawls *crawls.Repository
	runs   *crawls.RunRepository
	kbs    *kb.Client

	taxonomy *wsd.Taxonomy
	// queryInventory is the rich one (BabelNet) used once per run;
	// pageInventory is the local one used for every page. See internal/wsd.
	queryInventory wsd.Inventory
	pageInventory  wsd.Inventory

	crawler *crawler.Crawler
	// fetcher is kept so a web source can be built per run with that crawl's
	// own credentials and rules, while every one of them shares a single rate
	// limiter — politeness belongs to the site, not to the crawl.
	fetcher               *crawler.Fetcher
	allowPrivateAddresses bool
	salientTerms          int
	entityWeight          float64
	nameMissPenalty       float64
	vault                 crawls.Sealer
	recognizer            crawler.NameRecognizer
	runTimeout            time.Duration
	logger                *logging.Logger
}

// Handle executes one queued run.
//
// Returning nil acknowledges the message. A crawl that FAILED is not an error
// here — it is a recorded outcome — so only an inability to record the
// outcome is worth leaving pending for a retry.
func (r *Runner) Handle(ctx context.Context, msg queue.CrawlRunMessage) error {
	logger := r.logger.WithContext(ctx).WithFields("run_id", msg.RunID)

	run, found, err := r.runs.GetUnscoped(ctx, msg.RunID)
	if err != nil {
		return fmt.Errorf("crawl runner: read run %s: %w", msg.RunID, err)
	}
	if !found {
		// The crawl was deleted, taking its runs with it. Nothing to do and
		// nothing to retry.
		logger.Warn("run record is gone; dropping the message")
		return nil
	}
	if run.Status != crawls.StatusQueued {
		// A redelivery. Re-running would crawl a site twice and could
		// duplicate knowledge-base writes.
		logger.Info("run is not queued; refusing to run it again", "status", run.Status)
		return nil
	}
	crawl, found, err := r.crawls.GetUnscoped(ctx, run.CrawlID)
	if err != nil {
		return fmt.Errorf("crawl runner: read crawl %s: %w", run.CrawlID, err)
	}
	if !found {
		logger.Warn("crawl is gone; marking the run failed", "crawl_id", run.CrawlID)
		return r.finish(ctx, run, crawls.StatusFailed, "the crawl was deleted before its run started")
	}

	run.Status = crawls.StatusRunning
	run.StartedAt = time.Now().UTC()
	if err := r.runs.Save(ctx, run); err != nil {
		// Nothing has run yet, so a retry is safe and correct.
		return fmt.Errorf("crawl runner: mark run %s running: %w", run.ID, err)
	}

	runCtx := ctx
	if r.runTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.runTimeout)
		defer cancel()
	}
	// The crawl acts as its owner throughout: the knowledge base it writes
	// into is theirs, and the KB API re-checks that user's permissions. This
	// one line is the runner's entire authority.
	runCtx = identity.WithUserID(runCtx, crawl.UserID)

	return r.execute(runCtx, ctx, logger, crawl, run)
}

// execute performs the crawl and records everything it produced.
//
// runCtx carries the impersonated identity and the run deadline; bookkeeping
// uses the plain ctx, so a run that times out can still write its own record.
func (r *Runner) execute(runCtx, ctx context.Context, logger *logging.Logger,
	crawl crawls.Crawl, run crawls.Run) error {

	logger = logger.WithFields("crawl_id", crawl.ID, "kb_id", crawl.KnowledgeBaseID)
	spentBefore := r.spent()

	// The seed pages are fetched before the query is read, because they are
	// the best evidence available for what it means: a query is a handful of
	// words, while the page the operator chose to start from is a whole
	// document about the topic. They are handed to the crawl below as well, so
	// the site is asked for them once.
	// The source is chosen here, once, and nothing below this line knows
	// which one it got. That is the whole of the abstraction: a crawl of a
	// website and a crawl of an issue tracker differ in how a reference is
	// parsed, retrieved and scoped, and in nothing else.
	src, err := r.buildSource(crawl)
	if err != nil {
		logger.Error("failed to build the crawl's source", "err", err, "source", crawl.Source)
		return r.finish(ctx, run, crawls.StatusFailed, err.Error())
	}
	defer src.Close()

	// Credentials are checked once, before the run spends anything. Without
	// this a rejected token looks like a site with nothing in it: every
	// reference fails, each with whatever error that source gives for "you may
	// not see this", and the run succeeds having stored nothing.
	if verifier, ok := src.(source.Verifier); ok {
		if err := verifier.Verify(runCtx); err != nil {
			logger.Error("the crawl's source rejected its credentials", "err", err,
				"source", src.Name())
			return r.finish(ctx, run, crawls.StatusFailed, err.Error())
		}
	}

	seeds := r.crawler.Prefetch(runCtx, src, crawl.SeedURLs)
	logger.Info("seeds retrieved", "source", src.Name(),
		"wanted", len(crawl.SeedURLs), "got", len(seeds))

	rel, err := crawler.PrepareRelevance(runCtx, r.taxonomy, r.queryInventory, r.pageInventory,
		crawl.Query, crawler.RelevanceConfig{
			Alpha: crawl.Alpha, SalientTerms: r.salientTerms,
			EntityWeight: r.entityWeight, Recognizer: r.recognizer,
			NameMissPenalty: r.nameMissPenalty,
			SeedText:        crawler.SeedContext(seeds),
		})
	if err != nil {
		// Out of sense-inventory allowance AND unable to fall back to the
		// local WordNet, which in practice means WordNet failed to load.
		// There is nothing to crawl towards, so the run pauses rather than
		// crawling blind — the next run will find the cached analysis and
		// cost nothing.
		if errors.Is(err, wsd.ErrInventoryExhausted) {
			logger.Warn("sense inventory exhausted during query analysis; pausing", "err", err)
			if rel != nil {
				run.Analysis = rel.Analysis()
			}
			run.Stats.RequestsSpent = r.spent() - spentBefore
			return r.finish(ctx, run, crawls.StatusPartial, err.Error())
		}
		logger.Error("failed to analyse the query", "err", err)
		return r.finish(ctx, run, crawls.StatusFailed, err.Error())
	}
	rel.SetRequestsSpent(r.spent() - spentBefore)
	logger.Info("query analysed",
		"terms", len(rel.Analysis().Terms), "expansion", len(rel.Analysis().Expansion),
		"requests_spent", r.spent()-spentBefore,
		"degraded", rel.Analysis().Degraded)
	if rel.Analysis().Degraded {
		// Worth its own line at warn level: the run will succeed and store
		// pages, so nothing else about it will look unusual, but it was
		// steered by a smaller vocabulary than the crawl was designed around.
		logger.Warn("query understood against the local WordNet; " +
			"the rich sense inventory was unavailable")
	}

	result, err := r.crawler.Run(runCtx, rel, src, crawler.Options{
		Seeds:          crawl.SeedURLs,
		MaxPages:       crawl.MaxPages,
		MaxDepth:       crawl.MaxDepth,
		MaxDuration:    crawl.MaxDuration(),
		ScoreThreshold: crawl.ScoreThreshold,
		Prefetched:     seeds,
	}, nil)
	if err != nil {
		logger.Error("crawl failed", "err", err)
		return r.finish(ctx, run, crawls.StatusFailed, err.Error())
	}

	run.Analysis = result.Analysis
	run.Graph = result.Graph
	run.Frontier = result.Frontier
	run.StopReason = result.StopReason
	run.Stats = statsFrom(result)
	run.Stats.RequestsSpent = r.spent() - spentBefore

	// Rebuild the knowledge base from what was found. Errors here are
	// recorded but do not discard the report: knowing what the crawl found is
	// useful even when storing it failed.
	sync, syncErr := r.replace(runCtx, logger, &crawl, result)
	run.Stats.Removed, run.Stats.Stored = sync.removed, sync.stored

	if err := r.crawls.Update(ctx, crawl); err != nil {
		logger.Error("failed to save the crawl's page map", "err", err)
	}

	status := crawls.StatusSucceeded
	message := ""
	switch {
	case syncErr != nil:
		status, message = crawls.StatusFailed, syncErr.Error()
	case result.Paused:
		status = crawls.StatusPartial
		message = fmt.Sprintf("paused: %s", result.StopReason)
	}
	logger.Info("crawl finished",
		"status", status, "stop_reason", result.StopReason, "fetched", result.Fetched,
		"stored", sync.stored, "removed", sync.removed, "frontier", len(result.Frontier),
		"duration_ms", result.Duration.Milliseconds())
	return r.finish(ctx, run, status, message)
}

// syncCounts is what a knowledge-base rebuild did: how many documents were
// deleted, and how many uploaded in their place.
type syncCounts struct {
	stored, removed int
}

// replace rebuilds the knowledge base from what this run found.
//
// Every document the crawl previously stored is deleted and every page this
// run kept is uploaded, so the knowledge base holds exactly the latest run's
// findings. Nothing is carried over: a page whose content is unchanged is
// deleted and uploaded again like any other.
//
// The wipe happens HERE rather than before the crawl starts, so that a run
// which fails to fetch anything leaves the previous corpus alone instead of
// destroying it and having nothing to put back.
//
// There is no atomic swap in the KB API, so a window remains where the
// knowledge base holds less than a full corpus. Both halves are also
// asynchronous — deletes and uploads are queued for the indexer — so during
// a rebuild it may briefly hold the old documents, the new ones, or a
// mixture. It converges once the queue drains. Nothing is mis-deleted in the
// meantime: deletes name the previous run's document ids, and this run's
// uploads mint new ones.
//
// It mutates crawl.Pages, which the caller then saves.
func (r *Runner) replace(ctx context.Context, logger *logging.Logger,
	crawl *crawls.Crawl, result crawler.Result) (syncCounts, error) {

	var counts syncCounts
	now := time.Now().UTC()

	// Out with the old. A delete that fails is logged and its page dropped
	// from the map anyway: keeping the record would mean trying to delete the
	// same document on every future run, and the alternative — leaving it in
	// the map — would make a stale document permanent.
	for pageURL, record := range crawl.Pages {
		if record.DocumentID == "" {
			continue
		}
		if _, err := r.kbs.DeleteDocument(ctx, crawl.KnowledgeBaseID, record.DocumentID); err != nil {
			logger.Warn("failed to remove a document during rebuild",
				"url", pageURL, "document_id", record.DocumentID, "err", err)
			continue
		}
		counts.removed++
	}
	crawl.Pages = map[string]crawls.PageRecord{}

	// In with the new.
	for _, page := range result.Stored {
		documentID, err := r.upload(ctx, crawl.KnowledgeBaseID, page)
		if err != nil {
			// One page failing to upload is not a failed crawl. There is no
			// retry next run to rely on any more — the next run re-uploads
			// everything regardless — so it is simply absent this time.
			logger.Warn("failed to upload a page", "url", page.URL, "err", err)
			continue
		}
		crawl.Pages[page.URL] = crawls.PageRecord{
			DocumentID: documentID, ContentHash: page.ContentHash, LastSeen: now,
		}
		counts.stored++
	}
	return counts, nil
}

// upload puts one page into the knowledge base and returns its document id.
//
// It goes through the KB API rather than the indexer's queue so that the
// knowledge base's own permission check, organization scoping and
// kind-to-type mapping all apply — publishing directly would bypass every one
// of them (ADR-0004).
func (r *Runner) upload(ctx context.Context, kbID string, page crawler.StoredPage) (string, error) {
	body, _, err := r.kbs.UploadDocuments(ctx, kbID, []requesthelper.UploadedFile{{
		Filename: filenameFor(page),
		Content:  []byte(page.Text),
	}})
	if err != nil {
		return "", err
	}
	var uploaded struct {
		Documents []struct {
			DocumentID string `json:"document_id"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(body, &uploaded); err != nil {
		return "", err
	}
	if len(uploaded.Documents) == 0 {
		return "", fmt.Errorf("crawl runner: upload of %s returned no document id", page.URL)
	}
	return uploaded.Documents[0].DocumentID, nil
}

// filenameFor names a stored page.
//
// The filename is denormalised onto every chunk and is the only human-readable
// label a retrieval knowledge base's document listing shows, so it is built
// from the page's title and URL rather than left to a UUID.
func filenameFor(page crawler.StoredPage) string {
	label := strings.TrimSpace(page.Title)
	if label == "" {
		if u, err := url.Parse(page.URL); err == nil {
			label = strings.TrimPrefix(u.Host+u.Path, "www.")
		}
	}
	if label == "" {
		label = "page"
	}
	label = strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return c
		case c == '-', c == '_', c == '.', c == ' ':
			return c
		}
		return '-'
	}, label)
	if len(label) > 120 {
		label = label[:120]
	}
	return strings.TrimSpace(label) + ".txt"
}

// finish writes the run's final state.
//
// A failure to save here is logged and swallowed: the crawl already happened,
// and retrying the message would crawl the site a second time to record an
// outcome that is already known.
func (r *Runner) finish(ctx context.Context, run crawls.Run, status, message string) error {
	run.Status = status
	run.Error = message
	run.FinishedAt = time.Now().UTC()
	if !run.StartedAt.IsZero() {
		run.Stats.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	}
	if err := r.runs.Save(ctx, run); err != nil {
		r.logger.Error("failed to record the run outcome; not retrying",
			"run_id", run.ID, "status", status, "err", err)
	}
	return nil
}

// spent reports the sense inventory's usage, when it keeps a count.
func (r *Runner) spent() int {
	if counter, ok := r.queryInventory.(interface{ Spent() int }); ok {
		return counter.Spent()
	}
	return 0
}

// statsFrom summarises a crawl result.
func statsFrom(result crawler.Result) crawls.Stats {
	stats := crawls.Stats{Fetched: result.Fetched, DurationMS: result.Duration.Milliseconds()}
	for _, node := range result.Graph.Nodes {
		switch node.Status {
		case crawler.StatusStored:
			// Counted from the sync, which knows what was actually written.
		case crawler.StatusRejected:
			stats.Rejected++
		case crawler.StatusStopped:
			stats.Stopped++
		case crawler.StatusError:
			stats.Errors++
		}
	}
	return stats
}

// extractionRules converts the stored rules into the crawler's own type, which
// is separate so the crawler does not import the domain package.
func extractionRules(rules []crawls.ExtractionRule) []crawler.ExtractionRule {
	out := make([]crawler.ExtractionRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, crawler.ExtractionRule{
			URLPattern: rule.URLPattern, Selectors: rule.Selectors,
		})
	}
	return out
}

// credentialRules unseals a crawl's headers into the crawler's own type.
func credentialRules(vault crawls.Sealer, stored []crawls.CredentialRule) ([]crawler.CredentialRule, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	opened, err := crawls.OpenCredentials(vault, stored)
	if err != nil {
		return nil, err
	}
	out := make([]crawler.CredentialRule, 0, len(opened))
	for _, rule := range opened {
		out = append(out, crawler.CredentialRule{
			URLPattern: rule.URLPattern, Headers: rule.Headers,
		})
	}
	return out, nil
}

// buildSource turns a crawl's configuration into the space it explores.
//
// The credentials are unsealed here and nowhere else, and they never reach the
// report, the logs or the stored record. A crawl whose key no longer opens
// them fails rather than crawling unauthenticated: quietly fetching login
// pages and storing them as documents is a worse outcome than a visible error.
func (r *Runner) buildSource(crawl crawls.Crawl) (source.Source, error) {
	switch crawl.Source {
	case crawls.SourceJIRA:
		if crawl.JIRA == nil {
			return nil, fmt.Errorf("the crawl names the jira source but carries no jira configuration")
		}
		token, err := r.vault.Open(crawl.JIRA.Token)
		if err != nil {
			return nil, fmt.Errorf("the stored JIRA token could not be decrypted")
		}
		projects := crawl.JIRA.Projects
		if len(projects) == 0 {
			projects = jira.SeedProjects(crawl.SeedURLs)
		}
		return jira.New(jira.Config{
			BaseURL: crawl.JIRA.BaseURL, Email: crawl.JIRA.Email, Token: token,
			Projects: projects, MaxDepth: crawl.JIRA.MaxDepth,
			// The same switch the web fetcher honours, so one setting governs
			// every place the platform dials a user-chosen address.
			AllowPrivateAddresses: r.allowPrivateAddresses,
			Logger:                r.logger,
		})

	case crawls.SourceWeb, "":
		credentials, err := credentialRules(r.vault, crawl.Credentials)
		if err != nil {
			return nil, fmt.Errorf("the crawl's stored credentials could not be decrypted")
		}
		domains := crawl.AllowedDomains
		if len(domains) == 0 {
			domains = crawler.SeedDomains(crawl.SeedURLs)
		}
		return crawler.NewWebSource(r.fetcher, crawler.WebConfig{
			AllowedDomains:  domains,
			ExtractionRules: extractionRules(crawl.ExtractionRules),
			Credentials:     credentials,
		}), nil

	default:
		return nil, fmt.Errorf("unknown source %q", crawl.Source)
	}
}
