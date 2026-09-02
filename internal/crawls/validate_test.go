package crawls

import (
	"strings"
	"testing"
)

// valid is a crawl that passes, so each case can break exactly one thing.
func valid() Crawl {
	return ApplyDefaults(Crawl{
		Name:            "otters",
		Query:           "sea otter habitat",
		SeedURLs:        []string{"https://example.com/otters"},
		KnowledgeBaseID: "kb-1",
		IntervalMinutes: 60,
	})
}

func TestValidateAcceptsAWellFormedCrawl(t *testing.T) {
	if msg, ok := Validate(valid()); !ok {
		t.Fatalf("a valid crawl was rejected: %s", msg)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Crawl)
		says   string
	}{
		{"no name", func(c *Crawl) { c.Name = "  " }, "name is required"},
		{"no query", func(c *Crawl) { c.Query = "" }, "query is required"},
		{"no knowledge base", func(c *Crawl) { c.KnowledgeBaseID = "" }, "knowledge_base_id"},
		{"no seeds", func(c *Crawl) { c.SeedURLs = nil }, "at least one seed"},
		{"seed is not a URL", func(c *Crawl) { c.SeedURLs = []string{"not a url"} }, "must be absolute"},
		{"seed is relative", func(c *Crawl) { c.SeedURLs = []string{"/relative"} }, "must be absolute"},
		// A crawl fetches whatever its seed names, from the platform's own
		// network position. Restricting the scheme here is the readable half
		// of the rule the fetcher enforces at the socket.
		{"seed is a file URL", func(c *Crawl) { c.SeedURLs = []string{"file:///etc/passwd"} }, "http or https"},
		{"seed is a data URL", func(c *Crawl) { c.SeedURLs = []string{"data:text/html,<h1>x"} }, "http or https"},
		{"too many pages", func(c *Crawl) { c.MaxPages = MaxPagesCeiling + 1 }, "max_pages"},
		{"zero pages", func(c *Crawl) { c.MaxPages = 0 }, "max_pages"},
		{"too deep", func(c *Crawl) { c.MaxDepth = MaxDepthCeiling + 1 }, "max_depth"},
		{"too long", func(c *Crawl) { c.MaxDurationSec = MaxDurationCeiling + 1 }, "max_duration_seconds"},
		{"threshold above one", func(c *Crawl) { c.ScoreThreshold = 1.5 }, "score_threshold"},
		{"alpha above one", func(c *Crawl) { c.Alpha = 2 }, "alpha"},
		// Every 5 minutes against somebody else's site is abuse.
		{"interval too short", func(c *Crawl) { c.IntervalMinutes = 5 }, "interval_minutes"},
		{"domain given as a URL", func(c *Crawl) { c.AllowedDomains = []string{"https://example.com"} }, "bare domains"},
		{"empty domain", func(c *Crawl) { c.AllowedDomains = []string{""} }, "empty entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			msg, ok := Validate(c)
			if ok {
				t.Fatalf("accepted a crawl with %s", tc.name)
			}
			if !strings.Contains(msg, tc.says) {
				t.Errorf("message %q does not mention %q", msg, tc.says)
			}
		})
	}
}

// TestIntervalZeroIsManualOnly: unscheduled is a legitimate state, distinct
// from disabled — the crawl can still be run by hand.
func TestIntervalZeroIsManualOnly(t *testing.T) {
	c := valid()
	c.IntervalMinutes = 0
	if msg, ok := Validate(c); !ok {
		t.Errorf("interval 0 rejected: %s", msg)
	}
	if c.Interval() != 0 {
		t.Errorf("Interval() = %v, want 0", c.Interval())
	}
}

func TestApplyDefaults(t *testing.T) {
	c := ApplyDefaults(Crawl{
		Name: "x", Query: "y", KnowledgeBaseID: "kb",
		SeedURLs: []string{"https://docs.example.com/start"},
	})
	if c.MaxPages != DefaultMaxPages || c.MaxDepth != DefaultMaxDepth {
		t.Errorf("bounds not defaulted: %+v", c)
	}
	if c.ScoreThreshold != DefaultScoreThreshold || c.Alpha != 0.7 {
		t.Errorf("scoring not defaulted: threshold=%v alpha=%v", c.ScoreThreshold, c.Alpha)
	}
	// The default that makes "focused" mean something: without it the first
	// outbound link takes the crawl off the site it was pointed at.
	if len(c.AllowedDomains) != 1 || c.AllowedDomains[0] != "example.com" {
		t.Errorf("AllowedDomains = %v, want the seed's registrable domain", c.AllowedDomains)
	}
}

func TestApplyDefaultsNormalizesAndDedupesSeeds(t *testing.T) {
	c := ApplyDefaults(Crawl{
		SeedURLs: []string{
			"https://example.com/start",
			"https://example.com/start/",             // same page
			"https://example.com/start#section",      // same page
			"https://example.com/start?utm_source=x", // same page
			"https://example.com/other",
		},
	})
	if len(c.SeedURLs) != 2 {
		t.Errorf("seeds = %v, want the four spellings of one page collapsed to one", c.SeedURLs)
	}
}

func TestApplyDefaultsKeepsAnExplicitAllowlist(t *testing.T) {
	c := ApplyDefaults(Crawl{
		SeedURLs:       []string{"https://example.com/a"},
		AllowedDomains: []string{"example.com", "cdn.example.org"},
	})
	if len(c.AllowedDomains) != 2 {
		t.Errorf("an explicit allowlist was overwritten: %v", c.AllowedDomains)
	}
}

func TestMaxDurationFallsBackWhenUnset(t *testing.T) {
	if got := (Crawl{}).MaxDuration(); got != DefaultMaxDuration {
		t.Errorf("MaxDuration() = %v, want %v", got, DefaultMaxDuration)
	}
	if got := (Crawl{MaxDurationSec: 30}).MaxDuration().Seconds(); got != 30 {
		t.Errorf("MaxDuration() = %vs, want 30s", got)
	}
}

// TestValidateExtractionRules keeps a broken rule a 400 at creation rather
// than silence at three in the morning: a selector that never compiled would
// otherwise leave a site extracting nothing for a week with no error anywhere.
func TestValidateExtractionRules(t *testing.T) {
	base := func(rules []ExtractionRule) Crawl {
		c := Crawl{
			Name: "n", Query: "q", KnowledgeBaseID: "kb",
			SeedURLs: []string{"https://example.com/start"},
			MaxPages: 10, MaxDepth: 2, MaxDurationSec: 60,
			ExtractionRules: rules,
		}
		return c
	}
	valid := []ExtractionRule{{URLPattern: "https://jira.example.com/browse/*",
		Selectors: []string{"#issue h1", ".description p"}}}
	if msg, ok := Validate(base(valid)); !ok {
		t.Fatalf("a valid rule was rejected: %s", msg)
	}
	if msg, ok := Validate(base(nil)); !ok {
		t.Fatalf("no rules at all must remain valid: %s", msg)
	}

	cases := []struct {
		what  string
		rules []ExtractionRule
		says  string
	}{
		{"no pattern", []ExtractionRule{{Selectors: []string{"p"}}}, "url_pattern is required"},
		{"no selectors", []ExtractionRule{{URLPattern: "*"}}, "at least one selector"},
		{"an empty selector", []ExtractionRule{{URLPattern: "*", Selectors: []string{" "}}}, "is empty"},
		{"a selector that does not compile",
			[]ExtractionRule{{URLPattern: "*", Selectors: []string{"div[unclosed"}}},
			"not a valid CSS selector"},
		{"too many rules", manyRules(MaxExtractionRules + 1), "at most"},
		{"too many selectors",
			[]ExtractionRule{{URLPattern: "*", Selectors: manySelectors(MaxSelectorsPerRule + 1)}},
			"at most"},
	}
	for _, tc := range cases {
		msg, ok := Validate(base(tc.rules))
		if ok {
			t.Errorf("%s: accepted", tc.what)
			continue
		}
		if !strings.Contains(msg, tc.says) {
			t.Errorf("%s: message %q does not mention %q", tc.what, msg, tc.says)
		}
	}
}

func manyRules(n int) []ExtractionRule {
	out := make([]ExtractionRule, n)
	for i := range out {
		out[i] = ExtractionRule{URLPattern: "*", Selectors: []string{"p"}}
	}
	return out
}

func manySelectors(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "p"
	}
	return out
}
