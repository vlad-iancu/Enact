package cases

import (
	"net/http"
	"time"

	"enact/internal/enacttests/utils"
)

// crawlRunIntakeCase covers the asynchronous half of the API: a run is
// accepted with 202 and a record, the record is readable, and it reaches a
// terminal state.
//
// The seed points at a domain that does not resolve, so the crawl finishes
// quickly without fetching anything from anybody. What is under test is the
// intake path — record written before publishing, queue, orchestrator picks
// it up, outcome recorded — not the crawling, which internal/crawler covers
// against a local httptest site.
type crawlRunIntakeCase struct {
	kb    utils.KBDTO
	crawl utils.CrawlDTO
}

func NewCrawlRunIntake() utils.TestCase { return &crawlRunIntakeCase{} }

func (c *crawlRunIntakeCase) Name() string { return "TestCrawls_RunIntake" }

func (c *crawlRunIntakeCase) Setup(t *utils.T) {
	c.kb = t.CreateKBOfKind("rag")
	c.crawl = t.CreateCrawl(utils.CreateCrawlBody("run intake", "sea otter habitat",
		"https://crawl-e2e-nonexistent.invalid/start", c.kb.ID))
}

func (c *crawlRunIntakeCase) Run(t *utils.T) {
	// A bare POST with no body and no content type: the obvious way to start
	// a run by hand, and it used to answer 415.
	var run utils.CrawlRunDTO
	st := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodPost,
		t.CrawlURL("/v1/crawls/"+c.crawl.ID+"/runs"), nil, &run)
	if st != http.StatusAccepted {
		t.Fatalf("trigger: got HTTP %d (%s), want 202", st, run.Error)
	}
	if run.ID == "" || run.CrawlID != c.crawl.ID {
		t.Fatalf("run record is wrong: %+v", run)
	}
	if run.Status != "queued" {
		t.Errorf("a fresh run is %q, want queued", run.Status)
	}

	// The orchestrator has to pick it up and record an outcome.
	var final utils.CrawlRunDTO
	t.Eventually(90*time.Second, "the run reaches a terminal state", func() (bool, string) {
		var got utils.CrawlRunDTO
		if s := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodGet,
			t.CrawlURL("/v1/runs/"+run.ID), nil, &got); s != http.StatusOK {
			return false, "run fetch returned HTTP " + http.StatusText(s)
		}
		switch got.Status {
		case "succeeded", "failed", "partial":
			final = got
			return true, ""
		}
		return false, "status is " + got.Status
	})

	// Whatever the outcome, the report's first half must be there: the query
	// was analysed before anything was fetched, and that analysis is the
	// thing that explains every later decision.
	if final.Analysis.Query != c.crawl.Query {
		t.Errorf("the run report does not carry its query: %q", final.Analysis.Query)
	}
	if len(final.Analysis.Terms) == 0 {
		t.Errorf("the run report has no disambiguated terms")
	}
	if len(final.Analysis.Expansion) == 0 {
		t.Errorf("the run report has no query expansion")
	}
	t.Logf("run %s -> %s (%s), %d terms, %d expanded, %d graph nodes",
		final.ID, final.Status, final.StopReason,
		len(final.Analysis.Terms), len(final.Analysis.Expansion), len(final.Graph.Nodes))

	// A run of a crawl that does not exist is not found, rather than a 500.
	var missing utils.CrawlRunDTO
	if s := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodGet,
		t.CrawlURL("/v1/runs/no-such-run"), nil, &missing); s != http.StatusNotFound {
		t.Errorf("unknown run: got HTTP %d, want 404", s)
	}

	// The history lists it.
	var history struct {
		Runs []utils.CrawlRunDTO `json:"runs"`
	}
	if s := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodGet,
		t.CrawlURL("/v1/crawls/"+c.crawl.ID+"/runs"), nil, &history); s != http.StatusOK {
		t.Fatalf("list runs: got HTTP %d, want 200", s)
	}
	found := false
	for _, r := range history.Runs {
		if r.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the run is missing from the crawl's history (%d runs)", len(history.Runs))
	}
}

func (c *crawlRunIntakeCase) TearDown(t *utils.T) {
	t.DeleteCrawl(c.crawl.ID)
	t.DeleteKB(c.kb.ID)
}

// ---------------------------------------------------------------------------

// crawlIsolationCase proves a crawl is invisible outside its organization,
// and absent rather than forbidden — "not yours" must be indistinguishable
// from "does not exist".
type crawlIsolationCase struct {
	kb    utils.KBDTO
	crawl utils.CrawlDTO
}

func NewCrawlIsolation() utils.TestCase { return &crawlIsolationCase{} }

func (c *crawlIsolationCase) Name() string { return "TestCrawls_Isolation" }

// stranger is a colleague: a member of the same organization who simply holds
// no rules over this crawl.
//
// Deliberately not a user with NO organization. That case is 403 by design,
// with an actionable message — there is no resource to hide and the fix is an
// administrator approving an organization request. The 404 rule is about
// somebody who could plausibly have been granted access and was not, and
// conflating the two is how this case first failed.
const crawlStranger = "crawl-isolation-stranger"

func (c *crawlIsolationCase) Setup(t *utils.T) {
	if err := t.Env.PlaceInOrganization(t.Context(), crawlStranger); err != nil {
		t.Fatalf("place the stranger in an organization: %v", err)
	}
	c.kb = t.CreateKBOfKind("rag")
	c.crawl = t.CreateCrawl(utils.CreateCrawlBody("private", "sea otter habitat",
		"https://example.com/otters", c.kb.ID))
}

func (c *crawlIsolationCase) Run(t *utils.T) {
	const stranger = crawlStranger

	// Every single-crawl route must be 404 for somebody else, never 403:
	// the existence of the crawl is itself none of their business.
	for _, probe := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/crawls/" + c.crawl.ID},
		{http.MethodPut, "/v1/crawls/" + c.crawl.ID},
		{http.MethodDelete, "/v1/crawls/" + c.crawl.ID},
		{http.MethodPost, "/v1/crawls/" + c.crawl.ID + "/runs"},
		{http.MethodGet, "/v1/crawls/" + c.crawl.ID + "/runs"},
	} {
		st := t.DoJSONAs(stranger, "enact-tests", utils.CrawlAudience,
			probe.method, t.CrawlURL(probe.path), nil, nil)
		if st != http.StatusNotFound {
			t.Errorf("%s %s as a stranger: got HTTP %d, want 404", probe.method, probe.path, st)
		}
	}

	// And it must not appear in their listing.
	var listing struct {
		Crawls []utils.CrawlDTO `json:"crawls"`
	}
	t.DoJSONAs(stranger, "enact-tests", utils.CrawlAudience, http.MethodGet,
		t.CrawlURL("/v1/crawls"), nil, &listing)
	for _, got := range listing.Crawls {
		if got.ID == c.crawl.ID {
			t.Errorf("a stranger's listing includes the crawl")
		}
	}

	// The owner still sees it, so the case is not passing because everything
	// is broken.
	if !t.ListCrawlIDs()[c.crawl.ID] {
		t.Errorf("the owner cannot see their own crawl")
	}
}

func (c *crawlIsolationCase) TearDown(t *utils.T) {
	t.DeleteCrawl(c.crawl.ID)
	t.DeleteKB(c.kb.ID)
}
