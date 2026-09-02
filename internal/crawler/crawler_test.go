package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"enact/internal/logging"
	"enact/internal/wsd"
)

func testTaxonomy(t *testing.T) *wsd.Taxonomy {
	t.Helper()
	dir := os.Getenv("WORDNET_DIR")
	if dir == "" {
		t.Skip("WORDNET_DIR not set; run `make wordnet` and export it")
	}
	tax, err := wsd.NewTaxonomy(wsd.Config{WordNetDir: dir})
	if err != nil {
		t.Fatalf("NewTaxonomy: %v", err)
	}
	return tax
}

// ---------------------------------------------------------------- frontier

func TestFrontierPopsBestFirst(t *testing.T) {
	f := NewFrontier(0)
	for _, c := range []Candidate{
		{ID: "https://a/1", Score: 0.2},
		{ID: "https://a/2", Score: 0.9},
		{ID: "https://a/3", Score: 0.5},
		{ID: "https://a/4", Score: 0.7},
	} {
		if !f.Push(c) {
			t.Fatalf("Push(%s) refused", c.ID)
		}
	}
	var got []float64
	for {
		c, ok := f.Pop()
		if !ok {
			break
		}
		got = append(got, c.Score)
	}
	want := []float64{0.9, 0.7, 0.5, 0.2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pop order = %v, want %v", got, want)
		}
	}
}

func TestFrontierDedupes(t *testing.T) {
	f := NewFrontier(0)
	if !f.Push(Candidate{ID: "https://a/1", Score: 0.5}) {
		t.Fatal("first push refused")
	}
	f.Push(Candidate{ID: "https://a/1", Score: 0.9})
	if f.Len() != 1 {
		t.Errorf("Len = %d, want 1: the same URL must never occupy two slots", f.Len())
	}
	if !f.Seen("https://a/1") {
		t.Error("Seen should report a URL that entered the frontier")
	}
	// Once retrieved, no score brings it back — that is what stops a crawl
	// cycling between two pages that link to each other.
	popped, _ := f.Pop()
	if f.Push(Candidate{ID: popped.ID, Score: 1}) {
		t.Error("a retrieved URL was queued again; a crawl would loop")
	}
}

// TestFrontierRaisesOnABetterSighting. A page is linked from several places and
// the sightings are not equally informative: priority is
// 0.6*parent + 0.4*hint, so the same URL found from a hub page scoring 0.05 and
// from an article scoring 0.8 arrives with very different priorities. Keeping
// only the first sighting records where a link was FOUND rather than the best
// evidence for it, and the first sighting is systematically the weaker one —
// hubs are reached early and score low on prose while linking to everything.
func TestFrontierRaisesOnABetterSighting(t *testing.T) {
	f := NewFrontier(0)
	f.Push(Candidate{ID: "https://a/target", Score: 0.10, Depth: 3, Hint: "read more"})
	f.Push(Candidate{ID: "https://a/other", Score: 0.50, Depth: 1})

	// A better route to the same page.
	if !f.Push(Candidate{ID: "https://a/target", Score: 0.80, Depth: 1, Hint: "OpenSearch query DSL"}) {
		t.Fatal("a better sighting was not accepted")
	}
	if f.Len() != 2 {
		t.Fatalf("Len = %d, want 2: raising must not duplicate the entry", f.Len())
	}
	best, _ := f.Peek()
	if best.ID != "https://a/target" || best.Score != 0.80 {
		t.Fatalf("Peek = %+v, want the raised candidate at 0.80", best)
	}
	// Depth and hint travel with the score: they describe the route credited.
	if best.Depth != 1 || best.Hint != "OpenSearch query DSL" {
		t.Errorf("raised candidate kept the rejected route's depth/hint: %+v", best)
	}

	// A worse sighting changes nothing, so discovery order cannot alter the
	// outcome and the run stays reproducible.
	f.Push(Candidate{ID: "https://a/target", Score: 0.20, Depth: 9, Hint: "here"})
	best, _ = f.Peek()
	if best.Score != 0.80 || best.Depth != 1 {
		t.Errorf("a worse sighting lowered the candidate: %+v", best)
	}

	// The heap is still correctly ordered after the fix-up.
	first, _ := f.Pop()
	second, _ := f.Pop()
	if first.ID != "https://a/target" || second.ID != "https://a/other" {
		t.Errorf("pop order = %s, %s; want the raised candidate first", first.ID, second.ID)
	}
}

// TestFrontierRequeuesAPausedCandidate. When an allowance runs out mid-page the
// loop puts that candidate back so the next run retries it. Push refuses it —
// it is already seen — so without Requeue the one page the crawl was holding is
// dropped from the persisted frontier and never retried.
func TestFrontierRequeuesAPausedCandidate(t *testing.T) {
	f := NewFrontier(0)
	f.Push(Candidate{ID: "https://a/1", Score: 0.7})
	popped, _ := f.Pop()

	if !f.Requeue(popped) {
		t.Fatal("Requeue refused the candidate the run paused on")
	}
	remaining := f.Remaining()
	if len(remaining) != 1 || remaining[0].ID != "https://a/1" {
		t.Fatalf("Remaining = %v, want the paused candidate back for the resume", remaining)
	}
}

// TestFrontierEvictionIsNotPermanent: a candidate dropped because the queue was
// full was never judged, so a later better route may bring it back. Leaving it
// in seen would make a capacity accident permanent.
func TestFrontierEvictionIsNotPermanent(t *testing.T) {
	f := NewFrontier(2)
	f.Push(Candidate{ID: "https://a/1", Score: 0.1})
	f.Push(Candidate{ID: "https://a/2", Score: 0.5})
	f.Push(Candidate{ID: "https://a/3", Score: 0.6}) // evicts a/1

	if f.Seen("https://a/1") {
		t.Error("an evicted candidate is still marked seen and can never return")
	}
	if !f.Push(Candidate{ID: "https://a/1", Score: 0.9}) {
		t.Fatal("an evicted candidate was refused on a better sighting")
	}
	best, _ := f.Peek()
	if best.ID != "https://a/1" {
		t.Errorf("Peek = %s, want the returned candidate at 0.9", best.ID)
	}
}

// TestFrontierDropsWorstWhenFull: a late but excellent link must displace an
// early mediocre one, or a big site's first page fills the queue forever.
func TestFrontierDropsWorstWhenFull(t *testing.T) {
	f := NewFrontier(3)
	f.Push(Candidate{ID: "https://a/1", Score: 0.1})
	f.Push(Candidate{ID: "https://a/2", Score: 0.2})
	f.Push(Candidate{ID: "https://a/3", Score: 0.3})
	f.Push(Candidate{ID: "https://a/4", Score: 0.9})

	if f.Len() != 3 {
		t.Fatalf("Len = %d, want the limit 3", f.Len())
	}
	remaining := f.Remaining()
	for _, c := range remaining {
		if c.ID == "https://a/1" {
			t.Error("the worst candidate survived; the limit evicted the wrong one")
		}
	}
	if remaining[0].Score != 0.9 {
		t.Errorf("best remaining = %.1f, want the newly-added 0.9", remaining[0].Score)
	}
}

func TestFrontierRemainingIsNonDestructive(t *testing.T) {
	f := NewFrontier(0)
	f.Push(Candidate{ID: "https://a/1", Score: 0.4})
	f.Push(Candidate{ID: "https://a/2", Score: 0.8})

	first := f.Remaining()
	if len(first) != 2 || first[0].Score != 0.8 {
		t.Fatalf("Remaining = %v, want both, best first", first)
	}
	// It is used to serialise a paused crawl, so it must not empty the queue.
	if f.Len() != 2 {
		t.Errorf("Remaining drained the frontier: Len = %d", f.Len())
	}
	if second := f.Remaining(); len(second) != 2 {
		t.Errorf("second Remaining returned %d, want 2", len(second))
	}
}

// ---------------------------------------------------------------- URLs

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://Example.COM/Page":                "https://example.com/Page",
		"https://example.com/page#section":        "https://example.com/page",
		"https://example.com/page/":               "https://example.com/page",
		"https://example.com":                     "https://example.com/",
		"https://example.com:443/page":            "https://example.com/page",
		"http://example.com:80/page":              "http://example.com/page",
		"https://example.com/p?utm_source=x&id=7": "https://example.com/p?id=7",
		"https://example.com/p?fbclid=abc":        "https://example.com/p",
		"https://example.com/p?b=2&a=1":           "https://example.com/p?a=1&b=2",
	}
	for in, want := range cases {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := NormalizeURL(u); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeCollapsesTheSamePage is the property that matters: the four
// ways a site offers one article must reduce to one crawl target.
func TestNormalizeCollapsesTheSamePage(t *testing.T) {
	variants := []string{
		"https://example.com/article",
		"https://example.com/article/",
		"https://example.com/article#intro",
		"https://example.com/article?utm_source=twitter",
	}
	seen := map[string]bool{}
	for _, v := range variants {
		u, _ := url.Parse(v)
		seen[NormalizeURL(u)] = true
	}
	if len(seen) != 1 {
		t.Errorf("the same article normalised to %d distinct URLs: %v", len(seen), seen)
	}
}

func TestSameSite(t *testing.T) {
	allowed := []string{"example.com"}
	cases := map[string]bool{
		"https://example.com/a":      true,
		"https://www.example.com/a":  true,
		"https://docs.example.com/a": true,
		"https://evil.com/a":         false,
		// The classic suffix-matching trap.
		"https://notexample.com/a":       false,
		"https://example.com.evil.com/a": false,
	}
	for raw, want := range cases {
		if got := SameSite(raw, allowed); got != want {
			t.Errorf("SameSite(%q) = %v, want %v", raw, got, want)
		}
	}
	// An empty allowlist means unrestricted.
	if !SameSite("https://anything.example/", nil) {
		t.Error("an empty allowlist should not restrict")
	}
}

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.example.com/a": "example.com",
		"https://example.com/a":     "example.com",
		"https://a.b.example.com/":  "example.com",
		"not a url":                 "",
	}
	for raw, want := range cases {
		if got := RegistrableDomain(raw); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestPathWords(t *testing.T) {
	cases := map[string]string{
		"https://e.com/wiki/Sea_otter":         "wiki Sea otter",
		"https://e.com/species/enhydra-lutris": "species enhydra lutris",
		"https://e.com/a/b/page.html":          "a b page",
		"https://e.com/":                       "",
	}
	for raw, want := range cases {
		if got := pathWords(raw); got != want {
			t.Errorf("pathWords(%q) = %q, want %q", raw, got, want)
		}
	}
}

// ---------------------------------------------------------------- extraction

func TestExtractSeparatesContentFromBoilerplate(t *testing.T) {
	const page = `<html><head><title>Sea otters</title></head><body>
<nav><a href="/home">Home</a><a href="/about">About us</a></nav>
<article><h1>Sea otters</h1>
<p>The sea otter is a marine mammal native to the coasts of the northern Pacific.
Otters hold hands while sleeping so that they do not drift apart from one another.</p>
<p>Read more about their <a href="/habitat">habitat and range</a>.</p></article>
<footer>Copyright boilerplate nobody wants indexed</footer></body></html>`

	base, _ := url.Parse("https://example.com/otters")
	doc, err := Extract(Page{URL: base.String(), Body: []byte(page)}, base)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if doc.Title != "Sea otters" {
		t.Errorf("Title = %q", doc.Title)
	}
	if !strings.Contains(doc.Text, "marine mammal") {
		t.Errorf("main content missing from text: %q", doc.Text)
	}
	if strings.Contains(doc.Text, "Copyright boilerplate") {
		t.Errorf("footer boilerplate leaked into the text: %q", doc.Text)
	}
	// Links come from the whole document, navigation included, because nav is
	// often the only route into a site.
	got := map[string]string{}
	for _, l := range doc.Links {
		got[l.URL] = l.Anchor
	}
	if got["https://example.com/habitat"] != "habitat and range" {
		t.Errorf("in-content link missing or mislabelled: %v", got)
	}
	if _, ok := got["https://example.com/about"]; !ok {
		t.Errorf("navigation link dropped: %v", got)
	}
}

func TestExtractResolvesAndFiltersLinks(t *testing.T) {
	const page = `<html><body>
<a href="/rel">relative</a>
<a href="https://other.com/abs">absolute</a>
<a href="mailto:x@y.com">mail</a>
<a href="javascript:void(0)">js</a>
<a href="#frag">fragment</a>
<a href="/rel">duplicate</a>
</body></html>`
	base, _ := url.Parse("https://example.com/dir/page")
	doc, err := Extract(Page{URL: base.String(), Body: []byte(page)}, base)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	urls := map[string]bool{}
	for _, l := range doc.Links {
		urls[l.URL] = true
	}
	if !urls["https://example.com/rel"] {
		t.Errorf("relative link not resolved: %v", urls)
	}
	if !urls["https://other.com/abs"] {
		t.Errorf("absolute link dropped: %v", urls)
	}
	for _, unwanted := range []string{"mailto:x@y.com", "javascript:void(0)"} {
		if urls[unwanted] {
			t.Errorf("%q should not be a crawl candidate", unwanted)
		}
	}
	if len(doc.Links) != 2 {
		t.Errorf("got %d links, want 2 (fragment and duplicate excluded): %v", len(doc.Links), urls)
	}
}

// ---------------------------------------------------------------- fetching

func TestFetcherRejectsPrivateAddresses(t *testing.T) {
	// The SSRF guard, with the default (secure) configuration.
	f := NewFetcher(FetchConfig{Timeout: 5 * time.Second})
	for _, target := range []string{
		"http://127.0.0.1:9200/",
		"http://localhost:9200/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		_, err := f.Get(context.Background(), target)
		if err == nil {
			t.Errorf("%s was fetched; the SSRF guard did not fire", target)
			continue
		}
		if !strings.Contains(err.Error(), "private address") {
			t.Errorf("%s failed with %v, want the private-address refusal", target, err)
		}
	}
}

func TestFetcherHonoursRobots(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>hello</p></body></html>")
	}))
	defer srv.Close()

	f := NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0, Timeout: 5 * time.Second})
	ctx := context.Background()

	if _, err := f.Get(ctx, srv.URL+"/allowed"); err != nil {
		t.Errorf("an allowed path failed: %v", err)
	}
	_, err := f.Get(ctx, srv.URL+"/private/secret")
	if err == nil {
		t.Fatal("a disallowed path was fetched")
	}
	if !strings.Contains(err.Error(), "robots") {
		t.Errorf("error = %v, want the robots refusal", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/private/secret"] != 0 {
		t.Error("the disallowed path was requested from the server anyway")
	}
	if hits["/robots.txt"] != 1 {
		t.Errorf("robots.txt fetched %d times, want exactly 1 (it should be cached)", hits["/robots.txt"])
	}
}

func TestFetcherRateLimitsPerHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>hi</p></body></html>")
	}))
	defer srv.Close()

	const delay = 120 * time.Millisecond
	f := NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: delay, Timeout: 5 * time.Second})
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := f.Get(ctx, fmt.Sprintf("%s/p%d", srv.URL, i)); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	// Three requests, two gaps.
	if elapsed := time.Since(start); elapsed < 2*delay {
		t.Errorf("three requests took %v, want at least %v — the host delay is not being applied",
			elapsed, 2*delay)
	}
}

// ---------------------------------------------------------------- scoring

// TestScoreLinkIsAlwaysFinite is the regression for a bug that lost an entire
// crawl's report on the first real run against Wikipedia.
//
// textOverlap normalised by the number of distinct usable words in an anchor.
// For an anchor whose every word is shorter than three characters — "»",
// "3D", "of" — that count is zero, so the division was 0/0 = NaN. A NaN
// priority is uniquely destructive: it compares false against everything, so
// container/heap silently stops being ordered, and encoding/json refuses to
// marshal it, so saving the run failed with "json: unsupported value: NaN"
// and the whole crawl's graph was lost.
func TestScoreLinkIsAlwaysFinite(t *testing.T) {
	tax := testTaxonomy(t)
	rel := prepareOtterRelevance(t, tax)

	anchors := []string{
		"", " ", "»", "3D", "of", "a b c", " ", "12 34",
		"sea otter habitat", "Privacy Policy",
	}
	for _, anchor := range anchors {
		for _, parent := range []float64{0, 0.5, 1} {
			got := rel.ScoreLink(parent, Link{URL: "https://example.com/x", Anchor: anchor})
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Errorf("ScoreLink(parent=%.1f, anchor=%q) = %v, which is not a number", parent, anchor, got)
			}
			if got < 0 || got > 1 {
				t.Errorf("ScoreLink(parent=%.1f, anchor=%q) = %v, outside [0,1]", parent, anchor, got)
			}
		}
	}
	// And the report it feeds must be serialisable, which is what actually
	// broke.
	c := Candidate{ID: "https://example.com/x", Score: rel.ScoreLink(0.5, Link{Anchor: "»"})}
	if _, err := json.Marshal(c); err != nil {
		t.Errorf("a candidate could not be marshalled: %v", err)
	}
}

// TestScoreLinkPrefersRelevantAnchors: the guard must not have flattened the
// signal it protects.
func TestScoreLinkPrefersRelevantAnchors(t *testing.T) {
	tax := testTaxonomy(t)
	rel := prepareOtterRelevance(t, tax)

	relevant := rel.ScoreLink(0.5, Link{URL: "https://e.com/a", Anchor: "sea otter habitat and range"})
	irrelevant := rel.ScoreLink(0.5, Link{URL: "https://e.com/b", Anchor: "privacy policy"})
	if !(relevant > irrelevant) {
		t.Errorf("an on-topic anchor scored %.4f, an off-topic one %.4f", relevant, irrelevant)
	}
	// The URL path carries the hint when the anchor does not — "read more"
	// pointing at /species/sea-otter/habitat says plenty.
	byPath := rel.ScoreLink(0.5, Link{URL: "https://e.com/species/sea-otter/habitat", Anchor: "read more"})
	bare := rel.ScoreLink(0.5, Link{URL: "https://e.com/legal/terms", Anchor: "read more"})
	if !(byPath > bare) {
		t.Errorf("a descriptive path scored %.4f, an unrelated one %.4f", byPath, bare)
	}
	// A relevant parent must still dominate: it is the only measured quantity.
	if rel.ScoreLink(1.0, Link{Anchor: "privacy policy"}) <= rel.ScoreLink(0.0, Link{Anchor: "sea otter habitat"}) {
		t.Error("the parent page's score should outweigh an anchor hint")
	}
}

func TestSane(t *testing.T) {
	// In-range values pass through; out-of-range ones clamp.
	for in, want := range map[float64]float64{
		0.5: 0.5, 0: 0, 1: 1, -1: 0, 2: 1,
	} {
		if got := sane(in); got != want {
			t.Errorf("sane(%v) = %v, want %v", in, got, want)
		}
	}
	// Every non-finite value collapses to 0, INCLUDING +Inf — which the clamp
	// rule alone would map to 1. A non-finite score can only come from a
	// division by zero somewhere, and handing a bug the maximum priority
	// would let it dominate the whole frontier; scoring it zero means the
	// crawl ignores that one link instead.
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := sane(in); got != 0 {
			t.Errorf("sane(%v) = %v, want 0 — a non-finite score is a bug, not a maximum", in, got)
		}
	}
}

// ---------------------------------------------------------------- the loop

// otterSite is a small site with pages of deliberately varying relevance to
// "sea otter habitat", plus the shapes a real crawl has to survive: an
// off-site link, a link that 404s, and a page with no readable content.
func otterSite(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/": `<html><head><title>Index</title></head><body><article>
<p>A directory of pages about marine wildlife and coastal ecology.</p>
<a href="/otter-habitat">sea otter habitat and range</a>
<a href="/otter-diet">what sea otters eat</a>
<a href="/accounting">quarterly accounting policy</a>
<a href="https://elsewhere.example/off">an off-site page</a>
<a href="/missing">a page that is not there</a>
</article></body></html>`,

		"/otter-habitat": `<html><head><title>Otter habitat</title></head><body><article>
<p>The sea otter is a marine mammal that lives along rocky coasts. Its habitat is the
kelp forest of the northern Pacific, where the otter forages among the rocks for
shellfish. Otter populations depend on the health of the coastal kelp beds and the
surrounding marine water. Habitat loss threatens the otter across much of its range.</p>
</article></body></html>`,

		"/otter-diet": `<html><head><title>Otter diet</title></head><body><article>
<p>The otter eats sea urchins, crabs and shellfish gathered from the sea floor. A
marine mammal of this size must eat a quarter of its body weight each day, so the
otter forages almost constantly among the coastal rocks and kelp.</p>
</article></body></html>`,

		"/accounting": `<html><head><title>Accounting</title></head><body><article>
<p>The quarterly accounting policy governs revenue recognition, deferred tax
liabilities and the treatment of intangible assets on the balance sheet. Auditors
review the ledger at the close of each fiscal quarter.</p>
</article></body></html>`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// prepareOtterRelevance builds a Relevance for "sea otter habitat" using the
// LOCAL WordNet for both the query and the pages.
//
// In production the query side is BabelNet; here it is WordNet so the test
// spends no API budget and never touches the network. The code path under
// test is the same — PrepareRelevance takes the two inventories separately
// precisely so either can be swapped.
func prepareOtterRelevance(t *testing.T, tax *wsd.Taxonomy) *Relevance {
	t.Helper()
	inv := wsd.NewWordNetInventory(tax)
	rel, err := PrepareRelevance(context.Background(), tax, inv, inv,
		"sea otter habitat and marine coastal range",
		RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30})
	if err != nil {
		t.Fatalf("PrepareRelevance: %v", err)
	}
	return rel
}

func TestCrawlPrefersRelevantPages(t *testing.T) {
	tax := testTaxonomy(t)
	srv := otterSite(t)
	rel := prepareOtterRelevance(t, tax)

	fetcher := NewFetcher(FetchConfig{
		AllowPrivateAddresses: true, HostDelay: 0, Timeout: 10 * time.Second,
	})
	c := New(fetcher, logging.New())

	host, _ := url.Parse(srv.URL)
	web := NewWebSource(fetcher, WebConfig{AllowedDomains: []string{host.Hostname()}})
	result, err := c.Run(context.Background(), rel, web, Options{
		Seeds:          []string{srv.URL + "/"},
		MaxPages:       10,
		MaxDepth:       3,
		ScoreThreshold: 0, // score everything, so the ORDER is what is tested
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	byURL := map[string]Node{}
	for _, n := range result.Graph.Nodes {
		byURL[n.URL] = n
	}
	habitat := byURL[srv.URL+"/otter-habitat"]
	diet := byURL[srv.URL+"/otter-diet"]
	accounting := byURL[srv.URL+"/accounting"]

	if habitat.Status != StatusStored || diet.Status != StatusStored {
		t.Errorf("otter pages were not stored: habitat=%s diet=%s", habitat.Status, diet.Status)
	}
	// The heart of it: an on-topic page must outscore an off-topic one.
	if !(habitat.Score > accounting.Score) {
		t.Errorf("habitat scored %.3f but accounting scored %.3f; relevance is not discriminating",
			habitat.Score, accounting.Score)
	}
	if !(diet.Score > accounting.Score) {
		t.Errorf("diet scored %.3f but accounting scored %.3f", diet.Score, accounting.Score)
	}
	t.Logf("scores: habitat=%.3f diet=%.3f accounting=%.3f",
		habitat.Score, diet.Score, accounting.Score)
	t.Logf("  habitat parts: semantic=%.3f lexical=%.3f coverage=%.3f",
		habitat.Semantic, habitat.Lexical, habitat.Coverage)

	// An off-site link is recorded as stopped, with the reason, rather than
	// silently dropped — the report is meant to show where the crawl chose
	// not to go.
	off := byURL["https://elsewhere.example/off"]
	if off.Status != StatusStopped || off.Reason != ReasonOffDomain {
		t.Errorf("off-site link recorded as %+v, want stopped/off-domain", off)
	}
	// A 404 is an error node, not a missing one.
	missing := byURL[srv.URL+"/missing"]
	if missing.Status != StatusError || missing.Reason != ReasonFetchFailed {
		t.Errorf("404 recorded as %+v, want error/fetch-failed", missing)
	}
	// Every fetched page contributes edges.
	if len(result.Graph.Edges) < 5 {
		t.Errorf("got %d edges, want at least the index page's five links", len(result.Graph.Edges))
	}
	if result.Analysis.Query == "" || len(result.Analysis.Terms) == 0 {
		t.Error("the run report is missing its query analysis")
	}
}

// TestCrawlThresholdStopsTheSearch is the focused crawler's defining
// behaviour: it stops when nothing promising is left, rather than fetching
// everything and filtering.
func TestCrawlThresholdStopsTheSearch(t *testing.T) {
	tax := testTaxonomy(t)
	srv := otterSite(t)
	rel := prepareOtterRelevance(t, tax)

	fetcher := NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0, Timeout: 10 * time.Second})
	c := New(fetcher, logging.New())
	host, _ := url.Parse(srv.URL)

	// A threshold above anything this site can score.
	web := NewWebSource(fetcher, WebConfig{AllowedDomains: []string{host.Hostname()}})
	result, err := c.Run(context.Background(), rel, web, Options{
		Seeds:          []string{srv.URL + "/"},
		MaxPages:       50,
		ScoreThreshold: 0.99,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.StopReason != ReasonBelowThreshold {
		t.Errorf("StopReason = %q, want %q", result.StopReason, ReasonBelowThreshold)
	}
	if len(result.Stored) != 0 {
		t.Errorf("stored %d pages despite an unreachable threshold", len(result.Stored))
	}
	// Stopping on the threshold means the crawl finished, not paused: there
	// is nothing worth resuming.
	if result.Paused {
		t.Error("a crawl that ran out of relevant links should not be marked paused")
	}
	if result.Fetched > 3 {
		t.Errorf("fetched %d pages before giving up; the threshold should stop it early", result.Fetched)
	}
}

// TestCrawlPageBudgetPausesWithAFrontier: running out of pages is not
// finishing. The frontier must survive so the next run continues.
func TestCrawlPageBudgetPausesWithAFrontier(t *testing.T) {
	tax := testTaxonomy(t)
	srv := otterSite(t)
	rel := prepareOtterRelevance(t, tax)

	fetcher := NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0, Timeout: 10 * time.Second})
	c := New(fetcher, logging.New())
	host, _ := url.Parse(srv.URL)

	web := NewWebSource(fetcher, WebConfig{AllowedDomains: []string{host.Hostname()}})
	result, err := c.Run(context.Background(), rel, web, Options{
		Seeds:          []string{srv.URL + "/"},
		MaxPages:       2, // the index plus one
		ScoreThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.StopReason != ReasonPageBudget {
		t.Errorf("StopReason = %q, want %q", result.StopReason, ReasonPageBudget)
	}
	if !result.Paused {
		t.Error("a page-budget stop must be a pause, not a finish")
	}
	if len(result.Frontier) == 0 {
		t.Fatal("no frontier was returned; the next run would restart from the seed")
	}
	if result.Fetched != 2 {
		t.Errorf("fetched %d pages, want exactly the budget of 2", result.Fetched)
	}
	// Resuming must not re-fetch what was already done.
	second, err := c.Run(context.Background(), rel, web, Options{
		MaxPages:       10,
		ScoreThreshold: 0,
	}, result.Frontier)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if second.Fetched == 0 {
		t.Error("the resumed run fetched nothing; the frontier did not carry over")
	}
	t.Logf("first run fetched %d, paused with %d queued; resume fetched %d",
		result.Fetched, len(result.Frontier), second.Fetched)
}

func TestFetcherRejectsNonHTMLAndOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "notreallyapng")
		case "/huge":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>"+strings.Repeat("x", 5000)+"</body></html>")
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>ok</body></html>")
		}
	}))
	defer srv.Close()

	f := NewFetcher(FetchConfig{
		AllowPrivateAddresses: true, HostDelay: 0, MaxPageBytes: 1000, Timeout: 5 * time.Second,
	})
	ctx := context.Background()

	if _, err := f.Get(ctx, srv.URL+"/image"); err == nil || !strings.Contains(err.Error(), "not an HTML") {
		t.Errorf("non-HTML error = %v, want the HTML refusal", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/huge"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversize error = %v, want the size refusal", err)
	}
}

// meteredInventory delegates to a real inventory for a fixed number of sense
// lookups and then refuses, which is how a metered inventory actually fails:
// partway through a query, not before it.
type meteredInventory struct {
	delegate wsd.Inventory
	budget   int
	err      error
	calls    int
}

func (m *meteredInventory) Senses(ctx context.Context, lemma, pos string) ([]wsd.Synset, error) {
	m.calls++
	if m.calls > m.budget {
		return nil, m.err
	}
	return m.delegate.Senses(ctx, lemma, pos)
}

func (m *meteredInventory) Synset(ctx context.Context, id string) (wsd.Synset, error) {
	if m.calls > m.budget {
		return wsd.Synset{}, m.err
	}
	return m.delegate.Synset(ctx, id)
}

const fallbackQuery = "sea otter habitat and marine coastal range"

// TestQueryFallsBackToTheLocalInventory is the guarantee that a spent
// allowance costs quality rather than the whole run.
func TestQueryFallsBackToTheLocalInventory(t *testing.T) {
	tax := testTaxonomy(t)
	local := wsd.NewWordNetInventory(tax)
	cfg := RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30}

	// The control: what the local inventory makes of the query on its own.
	control, err := PrepareRelevance(context.Background(), tax, local, local, fallbackQuery, cfg)
	if err != nil {
		t.Fatalf("control PrepareRelevance: %v", err)
	}
	if control.Analysis().Degraded {
		t.Fatal("the control ran on the local inventory directly; it is not a fallback")
	}

	// Exhausting at 0 and at 2 covers both shapes: an allowance that was
	// already spent when the run began, and one that ran out partway through
	// the query — the case where a naive implementation would splice two
	// inventories together.
	for _, budget := range []int{0, 2} {
		metered := &meteredInventory{
			delegate: local, budget: budget,
			err: fmt.Errorf("test: %w", wsd.ErrInventoryExhausted),
		}
		rel, err := PrepareRelevance(context.Background(), tax, metered, local, fallbackQuery, cfg)
		if err != nil {
			t.Fatalf("budget %d: PrepareRelevance: %v", budget, err)
		}
		if !rel.Analysis().Degraded {
			t.Errorf("budget %d: Degraded = false, want true so the report says why", budget)
		}
		// The whole analysis must be redone, not continued: the result has to
		// match the control exactly, or some senses came from the exhausted
		// inventory and the expansion would reference synsets the surviving
		// one cannot walk.
		got, want := rel.Analysis().Terms, control.Analysis().Terms
		if len(got) != len(want) {
			t.Fatalf("budget %d: %d terms, want %d", budget, len(got), len(want))
		}
		for i := range got {
			if got[i].SynsetID != want[i].SynsetID {
				t.Errorf("budget %d: term %q resolved to %q, want %q",
					budget, got[i].Term.Lemma, got[i].SynsetID, want[i].SynsetID)
			}
		}
		if len(rel.Analysis().Expansion) == 0 {
			t.Errorf("budget %d: no expansion, so the crawl has nothing to steer by", budget)
		}
		// The point of the fallback is a usable crawl, not a tidy report.
		score, err := rel.ScorePage(context.Background(),
			"The sea otter is a marine mammal of the northern Pacific coast.")
		if err != nil {
			t.Fatalf("budget %d: ScorePage: %v", budget, err)
		}
		if score.Total <= 0 {
			t.Errorf("budget %d: scored 0, so the fallback analysis is inert", budget)
		}
	}
}

// TestQueryDoesNotFallBackOnOtherErrors keeps the fallback narrow. A broken
// inventory is a fault to surface, not a reason to quietly crawl on a smaller
// vocabulary.
func TestQueryDoesNotFallBackOnOtherErrors(t *testing.T) {
	tax := testTaxonomy(t)
	local := wsd.NewWordNetInventory(tax)
	broken := &meteredInventory{delegate: local, budget: 0, err: fmt.Errorf("connection refused")}

	rel, err := PrepareRelevance(context.Background(), tax, broken, local, fallbackQuery,
		RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30})
	if err == nil {
		t.Fatal("PrepareRelevance succeeded; a transport failure must not be hidden")
	}
	if rel != nil && rel.Analysis().Degraded {
		t.Error("Degraded = true; the fallback fired for something other than an exhausted allowance")
	}
}

// TestQueryDoesNotFallBackToItself guards the case where a deployment has no
// second inventory to offer, which must report the exhaustion rather than
// retry the same refusal.
func TestQueryDoesNotFallBackToItself(t *testing.T) {
	tax := testTaxonomy(t)
	spent := &meteredInventory{
		delegate: wsd.NewWordNetInventory(tax), budget: 0,
		err: fmt.Errorf("test: %w", wsd.ErrInventoryExhausted),
	}
	_, err := PrepareRelevance(context.Background(), tax, spent, spent, fallbackQuery,
		RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30})
	if !errors.Is(err, wsd.ErrInventoryExhausted) {
		t.Fatalf("err = %v, want the exhaustion to be reported", err)
	}
}

// TestWordNetOnlyAnalysisIsNotDegraded pins what DISABLE_BABELNET means.
//
// The flag wires the same local inventory into both slots. That must read as
// the configured mode and not as a failure: `degraded` says the rich
// inventory was unavailable and the run fell back, which is a prompt to check
// an API key or a daily allowance. Reporting it on every run of a deployment
// that has deliberately turned BabelNet off would train people to ignore the
// one flag that tells them something went wrong.
func TestWordNetOnlyAnalysisIsNotDegraded(t *testing.T) {
	tax := testTaxonomy(t)
	local := wsd.NewWordNetInventory(tax)

	rel, err := PrepareRelevance(context.Background(), tax, local, local, fallbackQuery,
		RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30})
	if err != nil {
		t.Fatalf("PrepareRelevance: %v", err)
	}
	if rel.Analysis().Degraded {
		t.Error("Degraded = true; WordNet is the configured inventory here, not a fallback")
	}
	if len(rel.Analysis().Terms) == 0 || len(rel.Analysis().Expansion) == 0 {
		t.Fatalf("the analysis is empty: %d terms, %d expansion",
			len(rel.Analysis().Terms), len(rel.Analysis().Expansion))
	}
	score, err := rel.ScorePage(context.Background(),
		"The sea otter is a marine mammal of the northern Pacific coast.")
	if err != nil {
		t.Fatalf("ScorePage: %v", err)
	}
	if score.Total <= 0 {
		t.Error("scored 0; a WordNet-only crawl must still be able to rank pages")
	}
}

// TestBlindShareWeighsUnresolvedNamesHeavily is the arithmetic behind handing
// the decision to BM25 when the semantic half cannot see what a query is about.
func TestBlindShareWeighsUnresolvedNamesHeavily(t *testing.T) {
	name := wsd.Sense{Term: wsd.Term{Lemma: "opensearch", POS: wsd.POSNoun, Entity: true}} // no SynsetID
	known := func(lemma string) wsd.Sense {
		return wsd.Sense{Term: wsd.Term{Lemma: lemma, POS: wsd.POSNoun}, SynsetID: "n1"}
	}
	senses := []wsd.Sense{name, known("database"), known("documentation"),
		known("syntax"), known("query"), known("language")}

	// One unresolved word in six, but it carries an entity's weight: 3 of 8.
	if got := blindShare(senses, 3); got < 0.36 || got > 0.38 {
		t.Errorf("blindShare = %.3f, want 3/8 — the name's weight, not its head count", got)
	}
	// A query whose every word resolved is not blind at all.
	if got := blindShare(senses[1:], 3); got != 0 {
		t.Errorf("blindShare = %v with everything resolved, want 0", got)
	}
	// Repeats are one decision, not several.
	if got := blindShare(append(senses, name), 3); got < 0.36 || got > 0.38 {
		t.Errorf("blindShare = %.3f when the name repeats; a word counts once", got)
	}
}

// TestCoverageDropsWhenTheQueryNamesSomethingUnknown is the same property end
// to end, and the one that decided the crawl: with coverage stuck at 1.00 the
// semantic half kept full authority over a query it could not represent, and
// gave its highest score of the run to an article about the wrong database.
func TestCoverageDropsWhenTheQueryNamesSomethingUnknown(t *testing.T) {
	tax := testTaxonomy(t)
	inv := wsd.NewWordNetInventory(tax)
	page := "Sea otters live along the northern Pacific coast and feed on shellfish."
	cfg := RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30}

	coverageOf := func(query string) float64 {
		rel, err := PrepareRelevance(context.Background(), tax, inv, inv, query, cfg)
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		if _, err := rel.ScorePage(context.Background(), page); err != nil {
			t.Fatalf("%q: ScorePage: %v", query, err)
		}
		return rel.Analysis().Coverage
	}

	// Every word of this one is in the dictionary.
	known := coverageOf("marine mammal habitat and coastal range")
	// The same query, plus a product name the dictionary has never heard of.
	named := coverageOf("opensearch marine mammal habitat and coastal range")

	if known <= 0 {
		t.Fatalf("the control query scored coverage %v; the fixture is not measuring anything", known)
	}
	if named >= known {
		t.Errorf("coverage %.3f with an unknown name vs %.3f without; a query the semantic "+
			"half cannot represent must not claim it can judge it", named, known)
	}
}

// fixedRecognizer stands in for the ONNX model, so this covers the wiring
// without a hundred megabytes of artefact or a native library.
type fixedRecognizer struct {
	found map[string]bool
	calls int
}

func (f *fixedRecognizer) Names(text string) map[string]bool {
	f.calls++
	out := map[string]bool{}
	for name := range f.found {
		if strings.Contains(strings.ToLower(text), name) {
			out[name] = true
		}
	}
	return out
}

// TestRecognizerTeachesALowercaseQuery covers the wiring: a name only the
// recogniser can see reaches the query's weights and the run's report.
//
// The seed here is lowercase deliberately, so that every rule the crawler owns
// fails on it — "kafka" is in WordNet as Franz Kafka, so the dictionary says
// nothing; it is not capitalised, so neither the tagger nor the spelling rules
// do either. Only the recogniser can supply it.
//
// Worth being straight about what this does and does not prove. The shipped
// model is CASED and would find nothing here either; the fake stands for the
// contract, not for that model's behaviour. On the well-capitalised pages a
// crawl actually fetches, the rules and the model overlap almost entirely, and
// the model's measured advantage is precision rather than reach — seven names
// from a real page against sixty-eight, of which the rules' extra sixty-one
// were mostly furniture.
func TestRecognizerTeachesALowercaseQuery(t *testing.T) {
	tax := testTaxonomy(t)
	inv := wsd.NewWordNetInventory(tax)
	// "kafka" IS in WordNet — Franz Kafka — so no spelling or dictionary rule
	// will ever mark it. Only evidence from the corpus can.
	const query = "kafka streams processing"
	const seed = "teams run kafka in production for stream processing."

	base := RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30, SeedText: seed}
	plain, err := PrepareRelevance(context.Background(), tax, inv, inv, query, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.QueryTerms()["kafka"]; got != 1 {
		t.Fatalf("without a model kafka weighs %v, want 1 — the fixture must start with it unrecognised", got)
	}

	withModel := base
	recognizer := &fixedRecognizer{found: map[string]bool{"kafka": true}}
	withModel.Recognizer = recognizer
	taught, err := PrepareRelevance(context.Background(), tax, inv, inv, query, withModel)
	if err != nil {
		t.Fatal(err)
	}
	if got := taught.QueryTerms()["kafka"]; got <= 1 {
		t.Errorf("kafka weighs %v with the model, want more than an ordinary word", got)
	}
	if recognizer.calls == 0 {
		t.Error("the recognizer was never consulted")
	}
	// Ordinary words must not be swept up with it.
	for _, common := range []string{"stream", "processing"} {
		if got := taught.QueryTerms()[common]; got > 1 {
			t.Errorf("%q weighs %v; only names are weighted above 1", common, got)
		}
	}
	if names := taught.Analysis().Names; len(names) != 1 || names[0] != "kafka" {
		t.Errorf("report names = %v, want [kafka]", names)
	}
}

// TestNilRecognizerChangesNothing keeps the native dependency optional in fact
// and not merely in intent.
func TestNilRecognizerChangesNothing(t *testing.T) {
	tax := testTaxonomy(t)
	inv := wsd.NewWordNetInventory(tax)
	cfg := RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30,
		SeedText: "Teams run OpenSearch in production.", Recognizer: nil}
	rel, err := PrepareRelevance(context.Background(), tax, inv, inv,
		"opensearch cluster tuning", cfg)
	if err != nil {
		t.Fatalf("a nil recognizer must be usable: %v", err)
	}
	// The spelling rules still work on their own: the seed capitalises it.
	if got := rel.QueryTerms()["opensearch"]; got <= 1 {
		t.Errorf("opensearch weighs %v without a model; the seed page still names it", got)
	}
}

// TestPageMissingTheQuerysNameIsPenalised covers the case that weighting alone
// could not reach.
//
// Weighting a name above ordinary words is a nudge, not a filter: one name at
// 3 beside five ordinary words at 1 is 37% of the query's weight, so a long
// page saturating the other 63% wins without the name at all. Measured on
// "opensearch database documentation, syntax and query language", an article
// about building a document search tool with React and Supabase — never
// mentioning OpenSearch in its text — scored 0.706 lexically against 0.517 for
// a real OpenSearch article, on length alone.
func TestPageMissingTheQuerysNameIsPenalised(t *testing.T) {
	tax := testTaxonomy(t)
	inv := wsd.NewWordNetInventory(tax)
	const query = "opensearch database documentation and query language"
	// The seed names it, so the query learns that "opensearch" is a name.
	cfg := RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30,
		SeedText: "OpenSearch is a search engine. Teams run OpenSearch in production."}

	rel, err := PrepareRelevance(context.Background(), tax, inv, inv, query, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if names := rel.Analysis().Names; len(names) == 0 {
		t.Fatalf("the fixture must learn a name; got %v", names)
	}

	// Same subject, same vocabulary, one page names the thing and one does not.
	const about = "The database documentation covers query language syntax. " +
		"A database query is written in the query language described in the documentation."
	withName, err := rel.ScorePage(context.Background(), "OpenSearch. "+about)
	if err != nil {
		t.Fatal(err)
	}
	withoutName, err := rel.ScorePage(context.Background(), about)
	if err != nil {
		t.Fatal(err)
	}
	if withoutName.Total >= withName.Total {
		t.Errorf("a page that never mentions the name scored %.3f against %.3f for one that does; "+
			"a query naming something is asking about that thing",
			withoutName.Total, withName.Total)
	}

	// And the penalty must be switchable off, because it is a judgement about
	// intent rather than a fact about relevance.
	off := cfg
	off.NameMissPenalty = 1
	relOff, err := PrepareRelevance(context.Background(), tax, inv, inv, query, off)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := relOff.ScorePage(context.Background(), about)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Total <= withoutName.Total {
		t.Errorf("with the penalty disabled the same page scored %.3f, not more than %.3f",
			plain.Total, withoutName.Total)
	}
}

// TestIsDocumentURL keeps the crawler from spending requests on things it can
// never read.
func TestIsDocumentURL(t *testing.T) {
	cases := []struct {
		what string
		url  string
		want bool
	}{
		{"an ordinary page", "https://dev.to/user/some-article-1be5", true},
		{"a directory", "https://example.com/docs/guide/", true},
		{"a version in the path", "https://example.com/v1.2/guide", true},
		{"a query string only", "https://example.com/search?q=a.png", true},

		{"a plain image", "https://example.com/cover.png", false},
		{"upper case extension", "https://example.com/COVER.JPG", false},
		{"a stylesheet", "https://example.com/app.css", false},
		{"a pdf the fetcher would refuse anyway", "https://example.com/paper.pdf", false},

		// The case that motivated this: dev.to serves images through a resizing
		// proxy, and the extension is only visible once the path is decoded.
		{"an image behind a resizing proxy",
			"https://media2.dev.to/dynamic/image/width=1000,height=420,fit=cover,gravity=auto," +
				"format=auto/https%3A%2F%2Fdev-to-uploads.s3.amazonaws.com%2Fuploads%2Farticles%2F8j7kvp.png",
			false},
	}
	for _, tc := range cases {
		if got := isDocumentURL(tc.url); got != tc.want {
			t.Errorf("%s: isDocumentURL(%.70s) = %v, want %v", tc.what, tc.url, got, tc.want)
		}
	}
}

// TestExtractSkipsNonDocumentLinks covers it through the extractor, which is
// where the saving actually happens: a link never collected is a request never
// made, and the per-host delay never paid.
func TestExtractSkipsNonDocumentLinks(t *testing.T) {
	const page = `<html><body><article>
	  <a class="cover" href="https://media2.dev.to/dynamic/image/format=auto/https%3A%2F%2Fx.com%2Fa.png">cover</a>
	  <a href="/real-article-1be5">a real article</a>
	  <a href="/style.css">stylesheet</a>
	</article></body></html>`
	base, _ := url.Parse("https://dev.to/user/post")
	doc, err := Extract(Page{Body: []byte(page), URL: base.String(), FinalURL: base.String()}, base)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range doc.Links {
		got = append(got, l.URL)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "/real-article-1be5") {
		t.Errorf("collected %v, want only the article", got)
	}
}

// TestExtractScoresTheTitle pins the fix for a real gap: trafilatura returns
// the title as metadata, so on a page whose article does not repeat it the
// title never reached Document.Text — and Text is the only thing scored,
// hashed and uploaded.
func TestExtractScoresTheTitle(t *testing.T) {
	body := strings.Repeat("<p>The allocator moves data between nodes until the load across the "+
		"cluster is even, and it keeps moving until every node holds a comparable amount. "+
		"This paragraph is long enough that the extractor takes its real path.</p>\n", 12)
	base, _ := url.Parse("https://example.com/a")

	t.Run("title absent from the body is added", func(t *testing.T) {
		doc, err := Extract(Page{
			Body:     []byte(`<html><head><title>Zanzibar quokka telemetry</title></head><body><article>` + body + `</article></body></html>`),
			URL:      base.String(),
			FinalURL: base.String(),
		}, base)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if !strings.Contains(doc.Text, "Zanzibar quokka telemetry") {
			t.Fatalf("title missing from the scored text: %.80q", doc.Text)
		}
	})

	t.Run("title already in the body is not doubled", func(t *testing.T) {
		doc, err := Extract(Page{
			Body: []byte(`<html><head><title>Zanzibar quokka telemetry</title></head><body><article>` +
				`<h1>Zanzibar quokka telemetry</h1>` + body + `</article></body></html>`),
			URL:      base.String(),
			FinalURL: base.String(),
		}, base)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if n := strings.Count(doc.Text, "Zanzibar quokka telemetry"); n != 1 {
			t.Fatalf("title appears %d times, want 1", n)
		}
	})
}
