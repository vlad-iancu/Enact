// Package crawls holds the focused-crawling domain: the definition of a
// crawl, the record of one run, and their repositories.
//
// It is a standalone package (ADR-0009) because two services share it —
// enact-crawls owns authoring and intake, enact-crawl-orchestrator owns
// scheduling and execution — and services never import each other.
//
// The algorithms live elsewhere: internal/crawler runs the search and
// internal/wsd scores relevance. This package is what gets stored.
package crawls

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"enact/internal/crawler"
	"enact/internal/opensearch"
)

// Config holds the OpenSearch index names for the domain.
type Config struct {
	Index     string `env:"OPENSEARCH_INDEX_CRAWLS, default=enact-crawls"`
	RunsIndex string `env:"OPENSEARCH_INDEX_CRAWL_RUNS, default=enact-crawl-runs"`
}

// Run statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	// StatusPartial: the crawl made progress but stopped with work left —
	// out of pages, time, or sense-inventory allowance. Its frontier is
	// stored and the next run continues from it. Not a failure.
	StatusPartial = "partial"
	StatusFailed  = "failed"
)

// Defaults for a new crawl. Every one of them bounds something that would
// otherwise be unbounded: pages fetched, depth explored, wall clock spent,
// and how picky the search is.
const (
	DefaultMaxPages       = 100
	DefaultMaxDepth       = 3
	DefaultMaxDuration    = 15 * time.Minute
	DefaultScoreThreshold = 0.25
	DefaultIntervalHours  = 24
)

// Crawl is a standing instruction: fetch pages about this query, starting
// here, into that knowledge base, on this schedule.
type Crawl struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// OrganizationID is stored rather than inferred from the owner: every
	// read compares it, and an organization owner bypasses permission checks,
	// so this is the only thing keeping one organization out of another's
	// data.
	OrganizationID string `json:"organization_id"`

	Name string `json:"name"`
	// Query is the natural-language description of what to look for. It is
	// what the whole search is aimed at, and it is editable — a crawl whose
	// query is wrong is fixed by rewriting it, not by starting over.
	Query string `json:"query"`
	// SeedURLs are where the crawl starts. Editable for the same reason.
	SeedURLs []string `json:"seed_urls"`
	// KnowledgeBaseID is the retrieval KB the crawl fills. Fixed after
	// creation: the crawl becomes that knowledge base's sole writer and keeps
	// a URL-to-document map for it (see Pages), which would be meaningless
	// against a different KB.
	KnowledgeBaseID string `json:"knowledge_base_id"`

	// AllowedDomains bounds where the crawl may go. Defaults to the
	// registrable domain of each seed, so a focused crawl does not wander
	// onto the open web.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	MaxPages       int      `json:"max_pages"`
	MaxDepth       int      `json:"max_depth"`
	MaxDurationSec int      `json:"max_duration_seconds"`
	// ScoreThreshold is both the filter and the stopping rule: pages below it
	// are not stored, and when the best unvisited link falls below it the
	// crawl is finished.
	ScoreThreshold float64 `json:"score_threshold"`
	// Alpha weights semantic similarity against BM25. See wsd.Combine.
	Alpha float64 `json:"alpha"`

	// IntervalMinutes is how often the crawl repeats; zero disables
	// scheduling without disabling the crawl, so it can still be run by hand.
	IntervalMinutes int       `json:"interval_minutes"`
	NextRunAt       time.Time `json:"next_run_at"`
	Enabled         bool      `json:"enabled"`

	// Pages maps each stored URL to the document the LAST run created for
	// it.
	//
	// A run replaces the knowledge base wholesale, so this is not an
	// incremental index: it exists so the next run knows which documents to
	// delete before uploading its own. Without it the KB API — which mints a
	// fresh document id per upload and has no upsert-by-URL — would leave the
	// knowledge base accumulating one copy of the corpus per run.
	// ExtractionRules override how a page's text is read, for sites the
	// general-purpose extractor cannot handle. Empty — the usual case — means
	// the extractor decides on its own. See ExtractionRule.
	ExtractionRules []ExtractionRule `json:"extraction_rules,omitempty"`

	// Credentials are the headers this crawl presents to sites that require
	// them. Values are sealed at rest and never returned by the API.
	Credentials []CredentialRule `json:"credentials,omitempty"`

	// Source names the space this crawl explores: "web" (the default) or
	// "jira". It decides how a seed is read, how a reference is retrieved and
	// what "in scope" means; everything else about a crawl is the same either
	// way, which is the point of the abstraction.
	Source string `json:"source,omitempty"`
	// JIRA configures the issue-tracker source. Ignored unless Source is
	// "jira". Its token is sealed like any other credential.
	JIRA *JIRAConfig `json:"jira,omitempty"`

	Pages map[string]PageRecord `json:"pages,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ExtractionRule tells the crawler where a page's text actually lives.
//
// The general-purpose extractor infers the main content from a document's
// shape, which works on articles and fails on applications: a JIRA ticket, a
// wiki, an admin console and a docs site built from divs have no <main> and no
// <article>, and what comes back is either nothing or the navigation. A rule
// says "on these URLs, the text is in these elements" and settles it.
//
// Rules apply to TEXT only. Links are still collected from the whole document,
// because a selector chosen to capture a ticket's description would otherwise
// also decide which links the crawl may follow, and those are different
// questions with different right answers.
type ExtractionRule struct {
	// URLPattern is a wildcard matched against the whole URL, where `*` stands
	// for any run of characters: `https://jira.example.com/browse/*`.
	URLPattern string `json:"url_pattern"`
	// Selectors are CSS selectors. Every element matching any of them
	// contributes its text, in document order, and a match inside another
	// match is not repeated.
	Selectors []string `json:"selectors"`
}

// CredentialRule is a set of request headers and the URLs they may be sent to.
//
// Scoped to a URL pattern rather than attached to the crawl as a whole, and
// that is the security property the type exists to carry. A crawl follows
// links, links leave sites, and a header attached to "this crawl" would be
// presented to whatever the crawl wandered onto — which for a JIRA token means
// handing it to a third party because somebody put a link in a ticket. Scoped
// to a pattern, the header goes where it was meant to go and nowhere else.
//
// The same reasoning applies to redirects, and there it has to be enforced at
// the transport: see crawler.credentialTransport.
type CredentialRule struct {
	// URLPattern is a wildcard matched against the whole URL, as in
	// ExtractionRule: `https://jira.example.com/*`.
	//
	// A pattern that would send a secret to any host is refused at validation:
	// there is no legitimate reason to present the same token to the entire
	// internet, and every reason not to.
	URLPattern string `json:"url_pattern"`
	// Headers are sent with requests to matching URLs. Values are SEALED in
	// storage and blanked on the way out, so a GET of a crawl returns the
	// header NAMES and nothing else — enough to see what a crawl is
	// configured to send, never enough to replay it.
	Headers map[string]string `json:"headers,omitempty"`
}

// Sources a crawl may explore.
const (
	SourceWeb  = "web"
	SourceJIRA = "jira"
)

// JIRAConfig points a crawl at an issue tracker.
type JIRAConfig struct {
	// BaseURL is the site: https://your-org.atlassian.net
	BaseURL string `json:"base_url"`
	// Email is the account the API token belongs to. Atlassian authenticates
	// with HTTP Basic where the username is the email, so both are needed.
	Email string `json:"email"`
	// Token is an Atlassian API token. Sealed at rest and blanked on the way
	// out, exactly like a credential header — because that is what it is.
	Token string `json:"token,omitempty"`
	// Projects restricts the crawl to these project keys. Empty means the
	// projects the seeds belong to.
	Projects []string `json:"projects,omitempty"`
	// MaxDepth bounds how far issue relationships are followed. Separate from
	// the crawl's own depth limit: that one bounds the search, this one bounds
	// how far a relationship is worth trusting, and issue graphs are dense
	// enough that the second is reached first.
	MaxDepth int `json:"max_depth,omitempty"`
}

// PageRecord is what the crawl remembers about one stored page between runs.
type PageRecord struct {
	// DocumentID is the knowledge-base document to delete at the start of the
	// next run's rebuild.
	DocumentID string `json:"document_id"`
	// ContentHash is of the extracted text. Recorded for the report and for
	// anything that wants to tell one version of a page from another; it no
	// longer decides whether to re-upload, because every run re-uploads.
	ContentHash string    `json:"content_hash"`
	LastSeen    time.Time `json:"last_seen"`
}

// MaxDuration is the crawl's wall-clock bound as a duration.
func (c Crawl) MaxDuration() time.Duration {
	if c.MaxDurationSec <= 0 {
		return DefaultMaxDuration
	}
	return time.Duration(c.MaxDurationSec) * time.Second
}

// Interval is the schedule as a duration; zero means unscheduled.
func (c Crawl) Interval() time.Duration {
	if c.IntervalMinutes <= 0 {
		return 0
	}
	return time.Duration(c.IntervalMinutes) * time.Minute
}

// Run is the record of one execution, and the report it produced.
type Run struct {
	ID             string `json:"id"`
	CrawlID        string `json:"crawl_id"`
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	Status         string `json:"status"`

	// Analysis is the first half of the report: the query POS-tagged,
	// disambiguated, and expanded.
	Analysis crawler.QueryAnalysis `json:"analysis"`
	// Graph is the second half: every document reached, its score, and the
	// links between them — including the frontier nodes where the search
	// stopped, and why.
	Graph crawler.Graph `json:"graph"`
	// Frontier is what was left unvisited, stored only for a partial run.
	//
	// Reported rather than resumed from: a run rebuilds the knowledge base
	// from scratch, and continuing from a previous frontier would fill it
	// with a later slice of the site instead of what the seeds lead to. It
	// says where this run's search stopped, which is what a page budget
	// bought.
	Frontier []crawler.Candidate `json:"frontier,omitempty"`

	Stats Stats  `json:"stats"`
	Error string `json:"error,omitempty"`
	// StopReason says why the crawl ended, in the crawler's vocabulary.
	StopReason string `json:"stop_reason,omitempty"`

	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Stats summarise a run at a glance, so a listing need not parse the graph.
type Stats struct {
	Fetched  int `json:"fetched"`
	Stored   int `json:"stored"`
	Rejected int `json:"rejected"`
	Stopped  int `json:"stopped"`
	Errors   int `json:"errors"`
	// Unchanged and Replaced belonged to the incremental sync that a
	// wholesale rebuild replaced. Kept so that runs recorded under the old
	// scheme still deserialise; never set by a current run.
	Unchanged int `json:"unchanged"`
	Replaced  int `json:"replaced"`
	// Removed is how many documents the rebuild deleted — the previous run's
	// corpus, in its entirety.
	Removed int `json:"removed"`
	// RequestsSpent is what the run cost at the sense inventory.
	RequestsSpent int   `json:"requests_spent"`
	DurationMS    int64 `json:"duration_ms"`
}

// Repository persists crawl definitions.
type Repository struct {
	os    *opensearch.Client
	index string
}

func NewRepository(os *opensearch.Client, cfg Config) *Repository {
	return &Repository{os: os, index: cfg.Index}
}

// EnsureIndex verifies the index exists; it is created by
// `make infrastructure-up`.
func (r *Repository) EnsureIndex(ctx context.Context) error {
	return ensureIndex(ctx, r.os, r.index)
}

func (r *Repository) Create(ctx context.Context, c Crawl) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, c.ID, body)
}

// Update replaces a crawl record.
func (r *Repository) Update(ctx context.Context, c Crawl) error { return r.Create(ctx, c) }

// Get fetches one crawl, scoped to an organization.
//
// A crawl belonging to a different organization is reported ABSENT rather
// than refused: callers render that as 404, and "not yours" must be
// indistinguishable from "does not exist".
func (r *Repository) Get(ctx context.Context, organizationID, id string) (Crawl, bool, error) {
	var c Crawl
	found, err := r.os.GetSource(ctx, r.index, id, &c)
	if err != nil || !found {
		return Crawl{}, found, err
	}
	if organizationID == "" || c.OrganizationID != organizationID {
		return Crawl{}, false, nil
	}
	return c, true, nil
}

// GetUnscoped fetches a crawl without an organization check.
//
// For the orchestrator alone, which has no organization of its own: it acts
// on records the API already authorized when it wrote them. No request-facing
// path may use this.
func (r *Repository) GetUnscoped(ctx context.Context, id string) (Crawl, bool, error) {
	var c Crawl
	found, err := r.os.GetSource(ctx, r.index, id, &c)
	if err != nil || !found {
		return Crawl{}, found, err
	}
	return c, true, nil
}

// List returns an organization's crawls, newest first. The caller then drops
// what their rules do not cover.
func (r *Repository) List(ctx context.Context, organizationID string) ([]Crawl, error) {
	if organizationID == "" {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"size":  1000,
		"query": map[string]any{"term": map[string]any{"organization_id": organizationID}},
		"sort":  []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("crawls: marshal list query: %w", err)
	}
	return r.search(ctx, body)
}

// Due returns the crawls whose next run is owed, oldest first.
//
// Unscoped by organization on purpose: the scheduler serves every
// organization, and a crawl's authority comes from the record's own UserID
// when the run executes, not from who asked for the sweep.
func (r *Repository) Due(ctx context.Context, now time.Time, limit int) ([]Crawl, error) {
	if limit <= 0 {
		limit = 50
	}
	body, err := json.Marshal(map[string]any{
		"size": limit,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"enabled": true}},
			map[string]any{"range": map[string]any{"next_run_at": map[string]any{"lte": now.UTC()}}},
		}}},
		"sort": []any{map[string]any{"next_run_at": map[string]any{"order": "asc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("crawls: marshal due query: %w", err)
	}
	return r.search(ctx, body)
}

func (r *Repository) search(ctx context.Context, body []byte) ([]Crawl, error) {
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Crawl, 0, len(hits))
	for _, h := range hits {
		var c Crawl
		if err := json.Unmarshal(h.Source, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	return r.os.DeleteDoc(ctx, r.index, id)
}

// RunRepository persists run records.
type RunRepository struct {
	os    *opensearch.Client
	index string
}

func NewRunRepository(os *opensearch.Client, cfg Config) *RunRepository {
	return &RunRepository{os: os, index: cfg.RunsIndex}
}

func (r *RunRepository) EnsureIndex(ctx context.Context) error {
	return ensureIndex(ctx, r.os, r.index)
}

// Save writes a run record whole.
func (r *RunRepository) Save(ctx context.Context, run Run) error {
	body, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return r.os.IndexDoc(ctx, r.index, run.ID, body)
}

func (r *RunRepository) Get(ctx context.Context, organizationID, id string) (Run, bool, error) {
	var run Run
	found, err := r.os.GetSource(ctx, r.index, id, &run)
	if err != nil || !found {
		return Run{}, found, err
	}
	if organizationID == "" || run.OrganizationID != organizationID {
		return Run{}, false, nil
	}
	return run, true, nil
}

// GetUnscoped is the orchestrator's read, for the same reason as
// Repository.GetUnscoped.
func (r *RunRepository) GetUnscoped(ctx context.Context, id string) (Run, bool, error) {
	var run Run
	found, err := r.os.GetSource(ctx, r.index, id, &run)
	if err != nil || !found {
		return Run{}, found, err
	}
	return run, true, nil
}

// ListByCrawl returns a crawl's runs, newest first.
func (r *RunRepository) ListByCrawl(ctx context.Context, organizationID, crawlID string, size int) ([]Run, error) {
	if organizationID == "" || crawlID == "" {
		return nil, nil
	}
	if size <= 0 || size > 200 {
		size = 50
	}
	body, err := json.Marshal(map[string]any{
		"size": size,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"organization_id": organizationID}},
			map[string]any{"term": map[string]any{"crawl_id": crawlID}},
		}}},
		"sort": []any{map[string]any{"queued_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("crawls: marshal run list query: %w", err)
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(hits))
	for _, h := range hits {
		var run Run
		if err := json.Unmarshal(h.Source, &run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// LatestPartial returns the most recent run of a crawl that paused with a
// frontier.
//
// Unused since runs stopped resuming (see Run.Frontier); kept because it is
// the natural read for "where did this crawl last run out of room", which a
// report or a future resume mode would want.
func (r *RunRepository) LatestPartial(ctx context.Context, crawlID string) (Run, bool, error) {
	body, err := json.Marshal(map[string]any{
		"size": 1,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"crawl_id": crawlID}},
			map[string]any{"term": map[string]any{"status": StatusPartial}},
		}}},
		"sort": []any{map[string]any{"finished_at": map[string]any{"order": "desc"}}},
	})
	if err != nil {
		return Run{}, false, err
	}
	hits, err := r.os.Search(ctx, r.index, body)
	if err != nil || len(hits) == 0 {
		return Run{}, false, err
	}
	var run Run
	if err := json.Unmarshal(hits[0].Source, &run); err != nil {
		return Run{}, false, err
	}
	return run, true, nil
}

// DeleteByCrawl removes a crawl's runs, for the delete cascade.
func (r *RunRepository) DeleteByCrawl(ctx context.Context, crawlID string) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"crawl_id": crawlID}},
	})
	if err != nil {
		return err
	}
	return r.os.DeleteByQuery(ctx, r.index, body)
}

func ensureIndex(ctx context.Context, os *opensearch.Client, index string) error {
	exists, err := os.IndexExists(ctx, index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("crawls: required index %q is missing; run `make infrastructure-up` to create it", index)
	}
	return nil
}
