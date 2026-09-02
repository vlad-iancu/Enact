package crawler

import (
	"net/url"
	"strings"
	"testing"
)

func TestMatchWildcard(t *testing.T) {
	cases := []struct {
		pattern, url string
		want         bool
	}{
		{"https://jira.example.com/browse/*", "https://jira.example.com/browse/ENG-1234", true},
		// The reason this is not path.Match: its `*` stops at a slash, and the
		// whole point of a URL pattern is to cross the separators below it.
		{"https://jira.example.com/browse/*", "https://jira.example.com/browse/a/b/c", true},
		{"https://jira.example.com/browse/*", "https://jira.example.com/dashboard", false},
		{"*/browse/*", "https://jira.example.com/browse/ENG-1", true},
		{"https://*.example.com/*", "https://wiki.example.com/page", true},
		{"https://*.example.com/*", "https://example.org/page", false},
		{"*", "https://anything.at.all/x", true},
		{"https://exact.example.com/page", "https://exact.example.com/page", true},
		{"https://exact.example.com/page", "https://exact.example.com/page2", false},
		// Hosts are case-insensitive and nobody typing a pattern thinks about
		// which half of a URL they are in.
		{"https://JIRA.example.com/browse/*", "https://jira.example.com/browse/X", true},
		{"", "https://example.com", false},
		// A trailing literal must actually end the URL.
		{"https://example.com/*.html", "https://example.com/a/b.html", true},
		{"https://example.com/*.html", "https://example.com/a/b.htmlx", false},
	}
	for _, tc := range cases {
		if got := matchWildcard(tc.pattern, tc.url); got != tc.want {
			t.Errorf("matchWildcard(%q, %q) = %v, want %v", tc.pattern, tc.url, got, tc.want)
		}
	}
}

// ticketHTML is the shape the feature exists for: an application page with no
// <main> and no <article>, where the content is divs and the navigation is
// bulkier than the text.
const ticketHTML = `<html><body>
  <nav><a href="/board">Board</a><a href="/backlog">Backlog</a>
       <p>Filters Dashboards People Apps Create Search Help Settings Profile</p></nav>
  <div id="jira-issue-header"><h1>ENG-1234 Indexing stalls under load</h1></div>
  <div class="issue-body">
    <div class="description">The indexer stops making progress when the queue exceeds
      ten thousand documents. Restarting the consumer clears it.</div>
    <script>var tracking = "should never be extracted";</script>
  </div>
  <div class="comments"><p>Reproduced on staging.</p></div>
  <footer>Powered by an issue tracker. Terms Privacy Cookies</footer>
</body></html>`

func extractWith(t *testing.T, body string, rules []ExtractionRule) Document {
	t.Helper()
	base, _ := url.Parse("https://jira.example.com/browse/ENG-1234")
	doc, err := Extract(Page{Body: []byte(body), URL: base.String(), FinalURL: base.String()},
		base, CompileRules(rules))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestExtractionRuleSelectsTheContent is the feature working on the case that
// motivated it.
func TestExtractionRuleSelectsTheContent(t *testing.T) {
	doc := extractWith(t, ticketHTML, []ExtractionRule{{
		URLPattern: "https://jira.example.com/browse/*",
		Selectors:  []string{"#jira-issue-header h1", ".description"},
	}})
	if !doc.Selected {
		t.Error("Selected = false; the report must be able to say a rule supplied the text")
	}
	for _, want := range []string{"ENG-1234", "indexer stops making progress"} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("extracted text is missing %q; got %q", want, doc.Text)
		}
	}
	// Everything the selectors did not name must be gone — that is the point.
	for _, unwanted := range []string{"Backlog", "Powered by", "Reproduced on staging"} {
		if strings.Contains(doc.Text, unwanted) {
			t.Errorf("extracted text contains %q, which no selector asked for; got %q",
				unwanted, doc.Text)
		}
	}
	// Script content inside a selected container is never text.
	if strings.Contains(doc.Text, "should never be extracted") {
		t.Errorf("script content leaked into the text: %q", doc.Text)
	}
	// Links still come from the WHOLE document: a selector chosen for text
	// must not decide where the crawl may go.
	var found bool
	for _, l := range doc.Links {
		if strings.HasSuffix(l.URL, "/backlog") {
			found = true
		}
	}
	if !found {
		t.Error("a link outside the selected elements was lost; links are collected document-wide")
	}
}

// TestNoRulesPreservesTheOldBehaviour is the compatibility promise.
func TestNoRulesPreservesTheOldBehaviour(t *testing.T) {
	withNone := extractWith(t, ticketHTML, nil)
	if withNone.Selected {
		t.Error("Selected = true with no rules configured")
	}
	// A rule whose pattern does not match must be equally inert.
	withOther := extractWith(t, ticketHTML, []ExtractionRule{{
		URLPattern: "https://wiki.example.com/*", Selectors: []string{".description"},
	}})
	if withOther.Selected || withOther.Text != withNone.Text {
		t.Errorf("a non-matching rule changed the extraction:\n got %q\nwant %q",
			withOther.Text, withNone.Text)
	}
}

// TestRuleThatSelectsNothingFallsBack covers the failure that matters
// operationally: a site redesign invalidates a selector, and the choice is
// between the old behaviour and empty pages.
func TestRuleThatSelectsNothingFallsBack(t *testing.T) {
	base := extractWith(t, ticketHTML, nil)
	doc := extractWith(t, ticketHTML, []ExtractionRule{{
		URLPattern: "https://jira.example.com/browse/*",
		Selectors:  []string{".this-class-no-longer-exists"},
	}})
	if doc.Selected {
		t.Error("Selected = true though the selectors matched nothing")
	}
	if doc.Text != base.Text {
		t.Errorf("a rule that selected nothing did not fall back:\n got %q\nwant %q",
			doc.Text, base.Text)
	}
}

// TestNestedSelectorsDoNotDoubleCount guards the term frequencies every score
// is built on: selecting a container and something inside it must not read the
// inner text twice.
func TestNestedSelectorsDoNotDoubleCount(t *testing.T) {
	doc := extractWith(t, ticketHTML, []ExtractionRule{{
		URLPattern: "*",
		Selectors:  []string{".issue-body", ".description"},
	}})
	if got := strings.Count(doc.Text, "indexer stops making progress"); got != 1 {
		t.Errorf("the nested text appears %d times, want 1", got)
	}
}

// TestFirstMatchingRuleWins makes a rule set behave like every routing table
// anyone has used: specific above general.
func TestFirstMatchingRuleWins(t *testing.T) {
	doc := extractWith(t, ticketHTML, []ExtractionRule{
		{URLPattern: "https://jira.example.com/browse/*", Selectors: []string{".description"}},
		{URLPattern: "*", Selectors: []string{"footer"}},
	})
	if strings.Contains(doc.Text, "Powered by") {
		t.Errorf("the general rule won over the specific one; got %q", doc.Text)
	}
}
