package crawler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"enact/internal/source"
	"sort"
	"strings"
	"time"

	"enact/internal/logging"
	"enact/internal/wsd"
)

// Node statuses in a crawl graph.
const (
	// StatusStored: fetched, scored above the threshold, kept.
	StatusStored = "stored"
	// StatusRejected: fetched and scored, but below the threshold.
	StatusRejected = "rejected"
	// StatusStopped: discovered and scored, never fetched. These are the
	// frontier's edges — where the search decided not to go, and why.
	StatusStopped = "stopped"
	// StatusError: fetching or extraction failed.
	StatusError = "error"
)

// Reasons a node was stopped or errored.
const (
	ReasonBelowThreshold = "below-threshold"
	ReasonPageBudget     = "page-budget"
	ReasonDepthLimit     = "depth-limit"
	ReasonTimeLimit      = "time-limit"
	ReasonOffDomain      = "off-domain"
	ReasonRobots         = "robots"
	ReasonSenseBudget    = "sense-budget"
	ReasonFetchFailed    = "fetch-failed"
	ReasonNoContent      = "no-content"
)

// Node is one page in the crawl graph.
type Node struct {
	URL      string  `json:"url"`
	Title    string  `json:"title,omitempty"`
	Status   string  `json:"status"`
	Depth    int     `json:"depth"`
	Score    float64 `json:"score"`
	Semantic float64 `json:"semantic"`
	Lexical  float64 `json:"lexical"`
	Coverage float64 `json:"coverage"`
	// ContentHash identifies the extracted text, so a repeat run can tell an
	// unchanged page from a changed one without re-uploading it.
	ContentHash string `json:"content_hash,omitempty"`
	// Selected records that an extraction rule supplied this page's text.
	Selected  bool   `json:"selected,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

// Edge is a link between two pages, and the priority it was given.
type Edge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Anchor   string  `json:"anchor,omitempty"`
	Priority float64 `json:"priority"`
	// Structural marks an edge followed because of the relationship rather
	// than the priority — an epic's child, a subtask, a parent. Recorded so a
	// reader can tell a link that earned its place from one that was
	// guaranteed a place, which the priority alone no longer distinguishes.
	Structural bool `json:"structural,omitempty"`
}

// Graph is the second half of a run's report: every document the crawl
// touched, what it scored, and how they link.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Result is everything one crawl produced.
type Result struct {
	Graph Graph
	// Stored are the pages worth keeping, in the order they were found.
	Stored []StoredPage
	// Frontier is what was left unvisited when the crawl stopped. Non-empty
	// only when the crawl paused rather than finished, and it is what makes
	// resuming possible.
	Frontier []source.Reference
	// Analysis is the query report.
	Analysis QueryAnalysis
	// Source names the implementation that was explored.
	Source string
	// Paused reports whether the crawl stopped early with work remaining.
	Paused bool
	// StopReason says why it stopped.
	StopReason string
	Fetched    int
	Duration   time.Duration
}

// StoredPage is a page the crawl decided to keep.
type StoredPage struct {
	URL         string
	Title       string
	Text        string
	ContentHash string
	Score       float64
}

// Options bounds one crawl.
type Options struct {
	Seeds          []string
	AllowedDomains []string
	MaxPages       int
	MaxDepth       int
	MaxDuration    time.Duration
	ScoreThreshold float64
	FrontierLimit  int
	// Prefetched are documents the caller already has, keyed by reference ID.
	// Used instead of retrieving, so reading the query against the seeds costs
	// no second request. See Crawler.Prefetch.
	Prefetched map[string]source.Document
}

// Crawler runs focused crawls.
type Crawler struct {
	fetch  *Fetcher
	logger *logging.Logger
}

func New(fetcher *Fetcher, logger *logging.Logger) *Crawler {
	return &Crawler{fetch: fetcher, logger: logger}
}

// Run performs one crawl and returns its graph, its kept pages and whatever
// frontier remained.
//
// The loop is best-first: take the most promising unvisited link anywhere,
// fetch it, score it, and enqueue its links at a priority derived from that
// score. It stops when the best remaining candidate is not worth fetching, or
// when a budget runs out.
//
// A budget running out is NOT a failure. The frontier is returned intact so
// the next run continues from here, which is what lets a crawl bounded by a
// daily sense-lookup allowance make progress every day instead of restarting
// from its seed.
func (c *Crawler) Run(ctx context.Context, rel *Relevance, src source.Source,
	opts Options, resume []source.Reference) (Result, error) {

	started := time.Now()
	result := Result{Analysis: rel.Analysis(), Source: src.Name()}

	frontier := NewFrontier(opts.FrontierLimit)
	nodes := map[string]*Node{}
	// Seeds enter at the maximum priority: they are the operator's explicit
	// instruction about where to start, not a guess.
	for _, seed := range resume {
		frontier.Push(seed)
	}
	for _, seed := range opts.Seeds {
		ref, err := src.Parse(seed)
		if err != nil {
			c.logger.Warn("skipping an unusable seed", "seed", seed, "err", err)
			continue
		}
		ref.Score = 1
		frontier.Push(ref)
	}

	deadline := time.Time{}
	if opts.MaxDuration > 0 {
		deadline = started.Add(opts.MaxDuration)
	}

	for {
		if reason, done := c.shouldStop(ctx, frontier, opts, result.Fetched, deadline); done {
			result.StopReason = reason
			// Anything still queued is where the search stopped, and the
			// report says so explicitly — the user asked for the frontier
			// edges, not just the pages visited.
			c.recordStopped(nodes, frontier, reason)
			result.Paused = pausedFor(reason)
			break
		}
		candidate, ok := frontier.Pop()
		if !ok {
			result.StopReason = "frontier-exhausted"
			break
		}

		node := &Node{URL: candidate.ID, Depth: candidate.Depth, Status: StatusError}
		nodes[candidate.ID] = node

		// A seed the caller already retrieved — to read the query against it —
		// is not retrieved again. Politeness is measured in requests made, and
		// asking a source for the same thing twice in one run to answer two of
		// our own questions is our problem to solve, not theirs.
		doc, prefetched := opts.Prefetched[candidate.ID]
		if !prefetched {
			var err error
			if doc, err = src.Retrieve(ctx, candidate); err != nil {
				node.Error = err.Error()
				node.Reason = classifyRetrievalError(err)
				c.logger.Debug("retrieval failed", "reference", candidate.ID, "err", err)
				// An exhausted allowance fails identically for every reference
				// after it, so it stops the run rather than burning the
				// frontier one hopeless retrieval at a time.
				if errors.Is(err, source.ErrExhausted) {
					node.Status = StatusStopped
					frontier.Requeue(candidate)
					result.StopReason = ReasonSenseBudget
					result.Paused = true
					c.recordStopped(nodes, frontier, ReasonSenseBudget)
					break
				}
				continue
			}
		}
		result.Fetched++
		if doc.Text == "" {
			node.Reason = ReasonNoContent
			continue
		}
		node.Title = doc.Title
		node.Selected = doc.Selected
		node.FetchedAt = time.Now().UTC().Format(time.RFC3339)

		score, err := rel.ScorePage(ctx, doc.ForScoring())
		if err != nil {
			// A sense-inventory budget error stops the crawl but keeps
			// everything already gathered; anything else is this page's
			// problem alone.
			if isBudgetError(err) {
				node.Status = StatusStopped
				node.Reason = ReasonSenseBudget
				frontier.Requeue(candidate) // unvisited again, for the resume
				result.StopReason = ReasonSenseBudget
				result.Paused = true
				c.recordStopped(nodes, frontier, ReasonSenseBudget)
				break
			}
			node.Error = err.Error()
			node.Reason = "scoring-failed"
			continue
		}
		node.Score, node.Semantic, node.Lexical, node.Coverage =
			score.Total, score.Semantic, score.Lexical, score.Coverage

		if score.Total < opts.ScoreThreshold {
			node.Status = StatusRejected
			node.Reason = ReasonBelowThreshold
		} else {
			node.Status = StatusStored
			node.ContentHash = hashText(doc.Text)
			result.Stored = append(result.Stored, StoredPage{
				URL: candidate.ID, Title: doc.Title, Text: doc.Text,
				ContentHash: node.ContentHash, Score: score.Total,
			})
		}

		// Links are enqueued even from a rejected page: a page can be a poor
		// answer and still be a good route — index and category pages are
		// exactly that, low on prose and rich in relevant links.
		c.enqueueLinks(&result, rel, src, frontier, nodes, candidate, doc, score.Total, opts)

		c.logger.Debug("crawl page scored",
			"reference", candidate.ID, "score", score.Total, "semantic", score.Semantic,
			"lexical", score.Lexical, "status", node.Status, "depth", candidate.Depth,
			"references", len(doc.References), "frontier", frontier.Len())
	}

	if result.Paused {
		result.Frontier = frontier.Remaining()
	}
	result.Graph.Nodes = collectNodes(nodes)
	result.Duration = time.Since(started)
	result.Analysis = rel.Analysis()
	return result, nil
}

// enqueueLinks scores a page's links and pushes the ones worth considering,
// recording every one as an edge whether or not it was queued.
func (c *Crawler) enqueueLinks(result *Result, rel *Relevance, src source.Source,
	frontier *Frontier, nodes map[string]*Node,
	parent source.Reference, doc source.Document, parentScore float64, opts Options) {

	childDepth := parent.Depth + 1
	for _, ref := range doc.References {
		priority := 0.0
		// Out-of-scope and too-deep references are recorded as stopped nodes
		// rather than dropped silently: "where did the crawl decide not to go"
		// is half of what a report is for.
		//
		// Scope is the SOURCE's judgement now, not a domain comparison the
		// loop makes: staying on a site and staying inside a project are the
		// same decision wearing different clothes.
		switch {
		case !src.Allows(ref):
			noteStopped(nodes, ref.ID, childDepth, ReasonOffDomain)
		case opts.MaxDepth > 0 && childDepth > opts.MaxDepth:
			noteStopped(nodes, ref.ID, childDepth, ReasonDepthLimit)
		default:
			priority = rel.ScoreLink(parentScore, Link{URL: ref.ID, Anchor: ref.Hint})
			// A structural reference is retrieved on the strength of the
			// relationship, so its priority is floored at the threshold: the
			// loop stops when the BEST remaining candidate is not worth
			// fetching, and a candidate at the threshold is, by definition.
			//
			// Floored rather than exempted, so best-first still holds. A
			// structural reference that also looks relevant keeps its higher
			// priority and is visited sooner; the rest queue up at the bar,
			// behind everything the query actually favours. Relevance has not
			// stopped mattering — it now decides only whether what comes back
			// is worth STORING, which for a piece of work whose parts were
			// deliberately grouped is the question that was worth asking.
			if ref.Structural && priority < opts.ScoreThreshold {
				priority = opts.ScoreThreshold
			}
			ref.Depth = childDepth
			ref.Score = priority
			frontier.Push(ref)
		}
		result.Graph.Edges = append(result.Graph.Edges, Edge{
			From: parent.ID, To: ref.ID, Anchor: ref.Hint,
			Priority: priority, Structural: ref.Structural,
		})
	}
}

// shouldStop decides whether the loop is finished, and why.
func (c *Crawler) shouldStop(ctx context.Context, frontier *Frontier, opts Options,
	fetched int, deadline time.Time) (string, bool) {

	if err := ctx.Err(); err != nil {
		return "cancelled", true
	}
	if opts.MaxPages > 0 && fetched >= opts.MaxPages {
		return ReasonPageBudget, true
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return ReasonTimeLimit, true
	}
	best, ok := frontier.Peek()
	if !ok {
		return "frontier-exhausted", true
	}
	// The focused crawler's own stopping rule: when the most promising thing
	// left is not promising enough, there is nothing worth fetching anywhere.
	// This is what distinguishes it from a crawler that merely filters — a
	// filter would keep fetching and discarding forever.
	if best.Score < opts.ScoreThreshold {
		return ReasonBelowThreshold, true
	}
	return "", false
}

// recordStopped turns whatever is left in the frontier into stopped nodes.
func (c *Crawler) recordStopped(nodes map[string]*Node, frontier *Frontier, reason string) {
	for _, candidate := range frontier.Remaining() {
		if _, seen := nodes[candidate.ID]; seen {
			continue
		}
		nodes[candidate.ID] = &Node{
			URL: candidate.ID, Depth: candidate.Depth, Status: StatusStopped,
			Score: candidate.Score, Reason: reason,
		}
	}
}

func noteStopped(nodes map[string]*Node, url string, depth int, reason string) {
	if _, seen := nodes[url]; seen {
		return
	}
	nodes[url] = &Node{URL: url, Depth: depth, Status: StatusStopped, Reason: reason}
}

// pausedFor reports whether a stop reason leaves work worth resuming.
//
// A crawl that ran out of pages, time or sense-lookups has more to do; one
// that ran out of relevant links has finished. The distinction decides
// whether the frontier is persisted.
func pausedFor(reason string) bool {
	switch reason {
	case ReasonPageBudget, ReasonTimeLimit, ReasonSenseBudget, "cancelled":
		return true
	}
	return false
}

// classifyRetrievalError names a failure in the report's vocabulary, from the
// source-neutral sentinels — so "robots.txt refused" and "the token cannot see
// this issue" arrive at the same place without the loop knowing which happened.
func classifyRetrievalError(err error) string {
	switch {
	case errors.Is(err, source.ErrForbidden):
		return ReasonRobots
	case errors.Is(err, source.ErrNotRetrievable):
		return ReasonNoContent
	case errors.Is(err, source.ErrOutOfScope):
		return ReasonOffDomain
	case errors.Is(err, source.ErrNotFound):
		return ReasonFetchFailed
	}
	return ReasonFetchFailed
}

// isBudgetError reports whether an error means the sense inventory is spent
// and the crawl should pause rather than fail.
//
// Matched against the contract's sentinel, not a concrete inventory's: the
// crawler is the general thing and the inventory the pluggable one, so
// depending on internal/babelnet here would invert that.
func isBudgetError(err error) bool {
	return errors.Is(err, wsd.ErrInventoryExhausted)
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func collectNodes(nodes map[string]*Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *n)
	}
	// Highest score first, so the report leads with what the crawl found.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && betterNode(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func betterNode(a, b Node) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.URL < b.URL
}

// Prefetch fetches and extracts a set of URLs ahead of the crawl proper.
//
// It exists so the seed pages can be read before the query is disambiguated:
// the words of the page a crawl was pointed at are the best available evidence
// for what its query means. The documents come back for the caller to use as
// context, and are handed to Run through Options.Prefetched so the site is
// asked only once.
//
// Failures are silent by design. A seed that cannot be fetched is a problem
// for the crawl, which will report it as a node with a reason when it reaches
// it in the ordinary way; it is not a reason to refuse to analyse the query.
//
// The rules apply here as much as in the crawl proper — more, arguably. The
// seed is the page whose text teaches the query its names, and it is handed
// back to Run through Options.Prefetched, so a seed extracted without its rule
// would be BOTH misread and stored misread.
func (c *Crawler) Prefetch(ctx context.Context, src source.Source, seeds []string) map[string]source.Document {
	out := make(map[string]source.Document, len(seeds))
	for _, seed := range seeds {
		ref, err := src.Parse(seed)
		if err != nil {
			c.logger.Debug("seed could not be parsed", "seed", seed, "err", err)
			continue
		}
		if _, done := out[ref.ID]; done {
			continue
		}
		doc, err := src.Retrieve(ctx, ref)
		if err != nil || doc.Text == "" {
			c.logger.Debug("seed prefetch failed", "reference", ref.ID, "err", err)
			continue
		}
		out[ref.ID] = doc
	}
	return out
}

// SeedContext joins prefetched documents into one body of text.
func SeedContext(docs map[string]source.Document) string {
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		parts = append(parts, doc.Title+"\n"+doc.ForScoring())
	}
	// Sorted so the same seeds always produce the same context, and therefore
	// the same disambiguation. Map order is not stable, and a report that
	// changed between identical runs would be untrustworthy.
	sort.Strings(parts)
	return strings.Join(parts, "\n\n")
}
