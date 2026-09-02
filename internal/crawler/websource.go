package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"enact/internal/source"
)

// WebConfig is everything that makes a web crawl a WEB crawl.
//
// These used to sit on the crawl loop's Options, where they did not belong: a
// domain allow-list, CSS selectors and HTTP headers are not things a search
// has, they are things the web has. Moving them here is most of the point of
// the abstraction — the loop's options are now bounds on the search, and
// nothing in them mentions HTTP.
type WebConfig struct {
	// AllowedDomains keeps a crawl on the site it was pointed at. Empty means
	// the registrable domains of the seeds.
	AllowedDomains []string
	// ExtractionRules override where a page's text is read from.
	ExtractionRules []ExtractionRule
	// Credentials are headers presented to sites that require them.
	Credentials []CredentialRule
}

// WebSource explores the web: references are URLs, retrieving is an HTTP GET,
// and the new references are the page's links.
type WebSource struct {
	fetch   *Fetcher
	rules   []compiledRule
	domains []string
}

// NewWebSource builds the web implementation of source.Source.
func NewWebSource(fetcher *Fetcher, cfg WebConfig) *WebSource {
	return &WebSource{
		fetch:   fetcher.Session(cfg.Credentials),
		rules:   CompileRules(cfg.ExtractionRules),
		domains: cfg.AllowedDomains,
	}
}

func (w *WebSource) Name() string { return "web" }

func (w *WebSource) Close() error { return nil }

// Parse accepts an absolute http or https URL and canonicalises it.
//
// Canonicalising here rather than at use is what makes Reference.ID a real
// identity: two spellings of one page must become one reference, or the crawl
// fetches it twice and the knowledge base stores it twice.
func (w *WebSource) Parse(seed string) (source.Reference, error) {
	trimmed := strings.TrimSpace(seed)
	u, err := url.Parse(trimmed)
	if err != nil {
		return source.Reference{}, fmt.Errorf("%q is not a URL: %w", trimmed, err)
	}
	if !u.IsAbs() {
		return source.Reference{}, fmt.Errorf("%q must be an absolute URL, including https://", trimmed)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return source.Reference{}, fmt.Errorf("%q must use http or https", trimmed)
	}
	return source.Reference{ID: NormalizeURL(u)}, nil
}

// SeedDomains returns the registrable domains of a set of seeds, for a crawl
// that did not name its own.
func SeedDomains(seeds []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seed := range seeds {
		if domain := RegistrableDomain(seed); domain != "" && !seen[domain] {
			seen[domain] = true
			out = append(out, domain)
		}
	}
	return out
}

// Allows keeps the crawl on its own site.
//
// SameSite rather than a comparison written here: it already means "this host
// or a subdomain of it", it is tested, and having one definition of on-site is
// the point. An earlier version of this method reimplemented it via
// RegistrableDomain — which takes a URL and parses it, so passing a bare
// hostname returned "" for both sides and quietly allowed everything.
func (w *WebSource) Allows(ref source.Reference) bool {
	return SameSite(ref.ID, w.domains)
}

// Retrieve fetches a page and returns its text and its links.
func (w *WebSource) Retrieve(ctx context.Context, ref source.Reference) (source.Document, error) {
	page, err := w.fetch.Get(ctx, ref.ID)
	if err != nil {
		return source.Document{}, webError(err)
	}
	base, err := url.Parse(page.FinalURL)
	if err != nil {
		base, _ = url.Parse(ref.ID)
	}
	// A redirect can land somewhere Allows was never asked about, so scope is
	// re-checked on the URL that actually answered.
	if !w.Allows(source.Reference{ID: page.FinalURL}) {
		return source.Document{}, fmt.Errorf("%w: redirected to %s", source.ErrOutOfScope, page.FinalURL)
	}
	doc, err := Extract(page, base, w.rules)
	if err != nil {
		return source.Document{}, fmt.Errorf("%w: %v", source.ErrNotRetrievable, err)
	}
	if strings.TrimSpace(doc.Text) == "" {
		return source.Document{}, source.ErrNotRetrievable
	}

	refs := make([]source.Reference, 0, len(doc.Links))
	for _, link := range doc.Links {
		refs = append(refs, source.Reference{ID: link.URL, Hint: link.Anchor})
	}
	return source.Document{
		Title: doc.Title, Text: doc.Text, References: refs, Selected: doc.Selected,
	}, nil
}

// webError maps the fetcher's failures onto the source sentinels, so the crawl
// can record why without knowing it is talking to the web.
func webError(err error) error {
	switch {
	case errors.Is(err, ErrBlockedByRobots), errors.Is(err, ErrPrivateAddress):
		return fmt.Errorf("%w: %v", source.ErrForbidden, err)
	case errors.Is(err, ErrNotHTML), errors.Is(err, ErrTooLarge):
		return fmt.Errorf("%w: %v", source.ErrNotRetrievable, err)
	}
	return err
}

// compile-time proof.
var _ source.Source = (*WebSource)(nil)
