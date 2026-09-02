package cases

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"enact/internal/enacttests/utils"
)

// crawlValidationCase covers what a crawl refuses at creation.
//
// The knowledge-base rules matter most. A crawl becomes its target's sole
// writer — it keeps a URL-to-document map so a repeat run can replace what
// changed — so it needs a knowledge base that is BOTH of the retrieval kind
// (a context one has no embeddings and nothing would retrieve from it) and
// EMPTY (otherwise the crawl's second run would start deleting documents
// somebody else uploaded).
type crawlValidationCase struct {
	ragKB     utils.KBDTO
	contextKB utils.KBDTO
	usedKB    utils.KBDTO
}

func NewCrawlValidation() utils.TestCase { return &crawlValidationCase{} }

func (c *crawlValidationCase) Name() string { return "TestCrawls_Validation" }

func (c *crawlValidationCase) Setup(t *utils.T) {
	c.ragKB = t.CreateKBOfKind("rag")
	c.contextKB = t.CreateKBOfKind("context")
	// A retrieval KB that already holds a document, to prove "empty" is
	// checked separately from "right kind".
	c.usedKB = t.CreateKBOfKind("rag")
	var upload struct {
		Error string `json:"error"`
	}
	if st := t.DoMultipart("enact-tests", utils.KBAudience,
		t.KBURL("/v1/knowledge-bases/"+c.usedKB.ID+"/documents"),
		"existing.txt", []byte("A document somebody uploaded by hand, about otters and rivers."),
		&upload); st != http.StatusAccepted {
		t.Fatalf("seed the non-empty kb: got HTTP %d (%s), want 202", st, upload.Error)
	}
	// Uploading is asynchronous: the document is queued, extracted, chunked
	// and embedded before it is visible. The emptiness check reads the
	// INDEXED state, so a case that created a crawl immediately would see an
	// empty knowledge base and pass for the wrong reason. (This is a real
	// property of the check, not just a test artifact — see the note in
	// enactcrawls.validateKnowledgeBase.)
	t.Eventually(60*time.Second, "the seeded document is indexed", func() (bool, string) {
		var detail utils.KBDTO
		if st := t.DoJSON("enact-tests", utils.KBAudience, http.MethodGet,
			t.KBURL("/v1/knowledge-bases/"+c.usedKB.ID), nil, &detail); st != http.StatusOK {
			return false, "detail returned HTTP " + http.StatusText(st)
		}
		for _, d := range detail.Documents {
			if d.Chunks > 0 {
				return true, ""
			}
		}
		return false, "not yet chunked"
	})
}

func (c *crawlValidationCase) Run(t *utils.T) {
	seed := "https://example.com/start"
	cases := []struct {
		what string
		body string
		says string
	}{
		{"no name", fmt.Sprintf(`{"name":"","query":"q","seed_urls":[%q],"knowledge_base_id":%q}`,
			seed, c.ragKB.ID), "name is required"},
		{"no query", fmt.Sprintf(`{"name":"n","query":"","seed_urls":[%q],"knowledge_base_id":%q}`,
			seed, c.ragKB.ID), "query is required"},
		{"no seeds", fmt.Sprintf(`{"name":"n","query":"q","seed_urls":[],"knowledge_base_id":%q}`,
			c.ragKB.ID), "at least one seed"},
		// A seed is the one thing a user types freehand that the platform
		// then fetches from its own network position.
		{"a file:// seed", fmt.Sprintf(`{"name":"n","query":"q","seed_urls":["file:///etc/passwd"],"knowledge_base_id":%q}`,
			c.ragKB.ID), "http or https"},
		{"a relative seed", fmt.Sprintf(`{"name":"n","query":"q","seed_urls":["/docs"],"knowledge_base_id":%q}`,
			c.ragKB.ID), "absolute"},
		{"an interval under the floor", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q,"interval_minutes":2}`,
			seed, c.ragKB.ID), "interval_minutes"},
		{"a missing knowledge base", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":"no-such-kb"}`, seed), "not found"},
		{"a context knowledge base", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q}`, seed, c.contextKB.ID),
			"needs one of kind"},
		// Extraction rules are compiled at creation so a typo cannot become a
		// week of silently empty documents.
		{"an extraction rule with no selectors", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q,`+
				`"extraction_rules":[{"url_pattern":"https://x.example.com/*"}]}`,
			seed, c.ragKB.ID), "at least one selector"},
		{"an extraction rule whose selector does not compile", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q,`+
				`"extraction_rules":[{"url_pattern":"*","selectors":["div[unclosed"]}]}`,
			seed, c.ragKB.ID), "not a valid CSS selector"},
		{"an extraction rule with no url pattern", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q,`+
				`"extraction_rules":[{"selectors":["p"]}]}`,
			seed, c.ragKB.ID), "url_pattern is required"},

		{"a knowledge base that is not empty", fmt.Sprintf(
			`{"name":"n","query":"q","seed_urls":[%q],"knowledge_base_id":%q}`, seed, c.usedKB.ID),
			"empty one"},
	}

	for _, tc := range cases {
		var out utils.CrawlDTO
		st := t.DoJSON("enact-tests", utils.CrawlAudience, http.MethodPost,
			t.CrawlURL("/v1/crawls"), strings.NewReader(tc.body), &out)
		if st != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", tc.what, st)
			// Defensively clean up anything wrongly accepted.
			t.DeleteCrawl(out.ID)
			continue
		}
		if !strings.Contains(out.Error, tc.says) {
			t.Errorf("%s: message %q does not mention %q", tc.what, out.Error, tc.says)
		}
	}

	// The positive control: the same shape against a suitable knowledge base
	// is accepted. Without it, a service that rejected everything would pass.
	ok := t.CreateCrawl(utils.CreateCrawlBody("valid", "otters", seed, c.ragKB.ID))
	t.DeleteCrawl(ok.ID)
}

func (c *crawlValidationCase) TearDown(t *utils.T) {
	t.DeleteKB(c.ragKB.ID)
	t.DeleteKB(c.contextKB.ID)
	t.DeleteKB(c.usedKB.ID)
}
