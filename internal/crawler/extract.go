package crawler

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/markusmobius/go-trafilatura"
	"golang.org/x/net/html"
)

// Document is a page reduced to what the crawler needs: its readable text,
// its title, and where it links.
type Document struct {
	Title string
	Text  string
	Links []Link
	// Selected records that an extraction rule chose this page's text, rather
	// than the general-purpose extractor inferring it. Reported per page,
	// because "the rule did not fire" and "the rule fired and found nothing"
	// look identical from a score and are fixed by opposite edits.
	Selected bool
}

// Link is an outbound reference, with the text that described it.
//
// Anchor text is kept because it is the only description of a page available
// before fetching it, and therefore the only thing besides the URL that can
// rank an unvisited link. A link labelled "sea otter diet" is worth following
// from a page about otters; one labelled "privacy policy" is not, and the two
// are indistinguishable by URL alone on many sites.
type Link struct {
	URL    string
	Anchor string
}

// MaxLinksPerPage bounds how many links one page contributes to the frontier.
//
// Some pages are link farms or paginated indexes with thousands of anchors.
// Without a cap, one such page fills the frontier with its own neighbourhood
// and the crawl never explores anything else — the frontier is a priority
// queue, not a fair one.
const MaxLinksPerPage = 300

// Extract pulls the main content out of an HTML page and collects its links.
//
// go-trafilatura does the content extraction: it is a port of the algorithm
// that scores highest on the standard boilerplate-removal benchmarks, and the
// difference from a naive text dump is the difference between scoring a page
// on its article and scoring it on its navigation menu.
func Extract(page Page, base *url.URL, rules ...[]compiledRule) (Document, error) {
	if len(page.Body) == 0 {
		return Document{}, fmt.Errorf("crawler: empty body for %s", page.URL)
	}
	result, err := trafilatura.Extract(bytes.NewReader(page.Body), trafilatura.Options{
		OriginalURL:  base,
		IncludeLinks: true,
		// Comments are other people's text, not the page's own, and a busy
		// comment section can outweigh the article it is attached to.
		ExcludeComments: true,
		ExcludeTables:   false,
	})
	if err != nil {
		return Document{}, fmt.Errorf("crawler: extract %s: %w", page.URL, err)
	}
	doc := Document{
		Text: strings.TrimSpace(result.ContentText),
	}

	// A matching rule replaces the inferred text. Only the text: the links
	// below still come from the whole document, because "where is this page's
	// content" and "where may the crawl go next" are different questions.
	//
	// A rule that matches the URL but selects nothing leaves the inferred text
	// in place. That is the safer failure: a site redesign that invalidates a
	// selector degrades to the old behaviour instead of silently emptying
	// every page it covers.
	if len(rules) > 0 && len(rules[0]) > 0 {
		if selectors := selectorsFor(rules[0], page.FinalURL); len(selectors) > 0 {
			if root, err := html.Parse(bytes.NewReader(page.Body)); err == nil {
				if text := selectedText(root, selectors); text != "" {
					doc.Text = text
					doc.Selected = true
				}
			}
		}
	}
	if result.Metadata.Title != "" {
		doc.Title = result.Metadata.Title
	}

	// The title joins the text, because the text is the only thing scoring and
	// the knowledge base ever see.
	//
	// trafilatura returns the title as METADATA, not as content. Measured: on
	// an article long enough to take the real extraction path, the title
	// reaches the body only if an <h1> happens to repeat it — otherwise the one
	// sentence the author wrote to say what the page is about was excluded from
	// the relevance function entirely, and from the stored document too.
	//
	// Prepended, since a title leads; and skipped when the body already carries
	// it, so the usual <h1> case is not counted twice.
	if doc.Title != "" && !strings.Contains(strings.ToLower(doc.Text), strings.ToLower(doc.Title)) {
		doc.Text = doc.Title + "\n" + doc.Text
	}

	// Links come from the WHOLE document, not just the extracted content.
	// Navigation is boilerplate for the purposes of reading a page, but it is
	// often the only route to the rest of a site — and a link that leads
	// nowhere relevant is handled by scoring it low, not by never seeing it.
	links, err := collectLinks(page.Body, base)
	if err != nil {
		return doc, err
	}
	doc.Links = links
	return doc, nil
}

// collectLinks walks the raw HTML for anchors, resolving each against the
// page's own URL.
func collectLinks(body []byte, base *url.URL) ([]Link, error) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("crawler: parse html: %w", err)
	}
	var (
		links []Link
		seen  = map[string]bool{}
		walk  func(*html.Node)
	)
	walk = func(n *html.Node) {
		if len(links) >= MaxLinksPerPage {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			if href, ok := attr(n, "href"); ok {
				if resolved, ok := resolveLink(base, href); ok && !seen[resolved] && isDocumentURL(resolved) {
					seen[resolved] = true
					links = append(links, Link{URL: resolved, Anchor: textOf(n)})
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return links, nil
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

// textOf flattens an element's descendant text, which for an anchor is its
// label.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// resolveLink turns an href into an absolute, normalised URL, reporting
// whether it is one the crawler should consider at all.
func resolveLink(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		// mailto:, javascript:, tel: and friends.
		return "", false
	}
	return NormalizeURL(resolved), true
}

// NormalizeURL produces the canonical form used to decide whether two links
// are the same page.
//
// Deduplication is what stops a crawl walking in circles: the same article is
// routinely reachable as "/page", "/page/", "/page#section" and
// "/page?utm_source=twitter", and treating those as four pages would spend
// the whole budget on one document. The fragment is dropped because it never
// reaches the server; tracking parameters are dropped because they identify
// the referrer, not the content.
func NormalizeURL(u *url.URL) string {
	out := *u
	out.Fragment = ""
	out.Scheme = strings.ToLower(out.Scheme)
	out.Host = strings.ToLower(out.Host)
	// Default ports are noise.
	if (out.Scheme == "http" && strings.HasSuffix(out.Host, ":80")) ||
		(out.Scheme == "https" && strings.HasSuffix(out.Host, ":443")) {
		out.Host = out.Host[:strings.LastIndex(out.Host, ":")]
	}
	if out.Path == "" {
		out.Path = "/"
	} else if len(out.Path) > 1 {
		out.Path = strings.TrimSuffix(out.Path, "/")
	}
	if q := out.Query(); len(q) > 0 {
		for key := range q {
			if isTrackingParam(key) {
				q.Del(key)
			}
		}
		// Encode() sorts keys, so parameter order stops mattering.
		out.RawQuery = q.Encode()
	}
	return out.String()
}

// isTrackingParam reports whether a query parameter describes how a visitor
// arrived rather than what they are looking at.
func isTrackingParam(key string) bool {
	k := strings.ToLower(key)
	if strings.HasPrefix(k, "utm_") {
		return true
	}
	switch k {
	case "fbclid", "gclid", "msclkid", "mc_cid", "mc_eid", "ref", "referrer",
		"source", "igshid", "_ga", "yclid":
		return true
	}
	return false
}

// SameSite reports whether a URL belongs to one of the allowed domains,
// matching a domain and any subdomain of it.
//
// The default for a crawl is the seed's own domain. A focused crawler that
// wanders onto the open web stops being focused: relevance decides the order
// of exploration, but something has to decide its extent, and "the site I
// pointed you at" is the boundary a user expects.
func SameSite(raw string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range allowed {
		d := strings.ToLower(strings.TrimSpace(domain))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// RegistrableDomain is a best-effort "example.com" from a URL, used to seed
// the default allowlist.
//
// Deliberately simple: it takes the last two labels, which is right for
// example.com and wrong for example.co.uk. Getting that fully right needs the
// public suffix list; being wrong here makes the default allowlist slightly
// too narrow (a crawl of bbc.co.uk would allow only co.uk-suffixed hosts it
// actually reaches), never too wide, and the creator can always pass an
// explicit list.
func RegistrableDomain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// nonDocument are the file extensions a crawl can never read.
//
// Not an optimisation — a correctness-shaped waste. The fetcher already
// refuses anything that is not HTML, but it refuses it AFTER the request, so
// every one of these costs a round trip, a slot behind the per-host delay, and
// somebody else's bandwidth. Measured on a dev.to crawl, 29 of 50 fetches were
// cover images: every article wraps its cover in
// `<a class="crayons-article__cover" href="…/image/…">`, so following links
// faithfully means asking for a picture once per article.
var nonDocument = map[string]bool{
	// images
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".svg": true, ".ico": true, ".bmp": true, ".tiff": true, ".avif": true,
	// audio and video
	".mp3": true, ".mp4": true, ".m4a": true, ".webm": true, ".ogg": true,
	".wav": true, ".mov": true, ".avi": true, ".mkv": true,
	// archives and binaries
	".zip": true, ".gz": true, ".tar": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true, ".dmg": true, ".exe": true, ".pkg": true,
	// assets
	".css": true, ".js": true, ".mjs": true, ".map": true, ".woff": true,
	".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	// documents the HTML-only fetcher would refuse anyway
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".rss": true, ".atom": true,
}

// isDocumentURL reports whether a link could plausibly be a readable page.
//
// The extension is read from the DECODED path, because a link is not always
// what it looks like: dev.to serves its images through a resizing proxy whose
// path is `/dynamic/image/width=1000,…,format=auto/` followed by the original
// URL, percent-encoded. Nothing about the raw path ends in an image extension;
// the decoded one ends in `.png`.
func isDocumentURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := u.Path
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	// The last dot of the last segment, so `/v1.2/guide` is not read as an
	// extension of ".2/guide".
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return true
	}
	return !nonDocument[strings.ToLower(path[dot:])]
}
