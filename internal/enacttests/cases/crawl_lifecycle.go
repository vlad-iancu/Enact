package cases

import (
	"fmt"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// crawlLifecycleCase covers a crawl's CRUD surface and the two fields it
// exists to have edited: the query and the seed URLs.
//
// It does not run a crawl. The crawl loop is exercised against an httptest
// site in internal/crawler's own tests, which is both faster and kinder than
// making the platform's e2e suite fetch pages from the open web on every run.
// What this proves is the SERVICE: validation, persistence, and the rules
// around the knowledge base.
type crawlLifecycleCase struct {
	kb    utils.KBDTO
	crawl utils.CrawlDTO
}

func NewCrawlLifecycle() utils.TestCase { return &crawlLifecycleCase{} }

func (c *crawlLifecycleCase) Name() string { return "TestCrawls_Lifecycle" }

func (c *crawlLifecycleCase) Setup(t *utils.T) {
	c.kb = t.CreateKBOfKind("rag")
}

func (c *crawlLifecycleCase) Run(t *utils.T) {
	c.crawl = t.CreateCrawl(utils.CreateCrawlBody(
		"e2e crawl", "sea otter habitat", "https://example.com/otters", c.kb.ID))

	// Defaults the service fills in. The allowlist is the important one: it
	// is what keeps a focused crawl on the site it was pointed at, and
	// nothing in the request asked for it.
	if len(c.crawl.AllowedDomains) != 1 || c.crawl.AllowedDomains[0] != "example.com" {
		t.Errorf("AllowedDomains = %v, want the seed's registrable domain", c.crawl.AllowedDomains)
	}
	if c.crawl.Alpha <= 0 {
		t.Errorf("alpha was not defaulted: %v", c.crawl.Alpha)
	}
	if !c.crawl.Enabled {
		t.Errorf("a new crawl should be enabled")
	}

	// Readable back.
	var fetched utils.CrawlDTO
	if st := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodGet,
		t.CrawlURL("/v1/crawls/"+c.crawl.ID), nil, &fetched); st != http.StatusOK {
		t.Fatalf("get crawl: got HTTP %d, want 200", st)
	}
	if fetched.Query != "sea otter habitat" {
		t.Errorf("query = %q", fetched.Query)
	}

	// The edit the feature is specified around.
	var updated utils.CrawlDTO
	body := `{"query":"sea otter diet and conservation",
	          "seed_urls":["https://example.com/otters","https://example.com/marine"],
	          "interval_minutes":60}`
	if st := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodPut,
		t.CrawlURL("/v1/crawls/"+c.crawl.ID), strings.NewReader(body), &updated); st != http.StatusOK {
		t.Fatalf("update crawl: got HTTP %d (%s), want 200", st, updated.Error)
	}
	if updated.Query != "sea otter diet and conservation" {
		t.Errorf("query was not updated: %q", updated.Query)
	}
	if len(updated.SeedURLs) != 2 {
		t.Errorf("seed URLs = %v, want both", updated.SeedURLs)
	}
	if updated.IntervalMinutes != 60 {
		t.Errorf("interval = %d, want 60", updated.IntervalMinutes)
	}
	// Scheduling it must give it a next run, or the sweep never picks it up.
	if updated.NextRunAt == "" || strings.HasPrefix(updated.NextRunAt, "0001-01-01") {
		t.Errorf("next_run_at = %q; a scheduled crawl must be due at some point", updated.NextRunAt)
	}

	// The knowledge base is deliberately immutable: the crawl holds a
	// URL-to-document map against it.
	var repoint utils.CrawlDTO
	repointBody := fmt.Sprintf(`{"knowledge_base_id":%q}`, "some-other-kb")
	if st := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodPut,
		t.CrawlURL("/v1/crawls/"+c.crawl.ID), strings.NewReader(repointBody), &repoint); st != http.StatusBadRequest {
		t.Errorf("repointing the knowledge base: got HTTP %d, want 400", st)
	}

	if !t.ListCrawlIDs()[c.crawl.ID] {
		t.Errorf("the crawl is missing from the listing")
	}
}

func (c *crawlLifecycleCase) TearDown(t *utils.T) {
	t.DeleteCrawl(c.crawl.ID)
	t.DeleteKB(c.kb.ID)
}
