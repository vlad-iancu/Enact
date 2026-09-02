package crawler

import (
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// ExtractionRule is a URL pattern and the selectors that find a page's text on
// the URLs it matches. It mirrors crawls.ExtractionRule; the crawler keeps its
// own copy so this package does not depend on the domain one.
type ExtractionRule struct {
	URLPattern string
	Selectors  []string
}

// compiledRule is a rule with its selectors compiled once per run rather than
// once per page.
type compiledRule struct {
	pattern   string
	selectors []cascadia.Selector
}

// CompileRules prepares a rule set for use.
//
// Selectors that do not compile are dropped rather than failing the crawl: the
// API rejects them at creation, so one reaching here means a rule was written
// against an older, laxer validator, and losing one selector is better than
// losing every page.
func CompileRules(rules []ExtractionRule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		compiled := compiledRule{pattern: rule.URLPattern}
		for _, selector := range rule.Selectors {
			if sel, err := cascadia.Compile(selector); err == nil {
				compiled.selectors = append(compiled.selectors, sel)
			}
		}
		if len(compiled.selectors) > 0 {
			out = append(out, compiled)
		}
	}
	return out
}

// selectorsFor returns the selectors of the FIRST rule matching a URL.
//
// First rather than all, so a rule set reads as an ordered list of special
// cases: put the specific pattern above the general one and it wins, the way
// every routing table anyone has used behaves.
func selectorsFor(rules []compiledRule, url string) []cascadia.Selector {
	for _, rule := range rules {
		if matchWildcard(rule.pattern, url) {
			return rule.selectors
		}
	}
	return nil
}

// matchWildcard matches a URL against a pattern in which `*` stands for any
// run of characters, including `/`.
//
// Deliberately not path.Match, whose `*` stops at a slash — that is the right
// rule for file paths and the wrong one for URLs, where the whole point of
// `https://jira.example.com/browse/*` is to cross the separators below it.
// Case-insensitive, because host names are and nobody typing a pattern thinks
// about which half of a URL they are in.
func matchWildcard(pattern, url string) bool {
	pattern, url = strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(url)
	if pattern == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	// No wildcard at all: an exact URL.
	if len(parts) == 1 {
		return pattern == url
	}
	// The text before the first `*` must start the URL, and the text after the
	// last must end it; everything between has to appear in order.
	if !strings.HasPrefix(url, parts[0]) {
		return false
	}
	rest := url[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(rest, part)
		if index < 0 {
			return false
		}
		rest = rest[index+len(part):]
	}
	return strings.HasSuffix(rest, last) && len(rest) >= len(last)
}

// selectedText gathers the text of every element matching any selector, in
// document order.
//
// A match nested inside another match is skipped, so selecting both a
// container and something within it does not read the inner text twice — which
// would double its term frequency and quietly bias every score the page gets.
func selectedText(root *html.Node, selectors []cascadia.Selector) string {
	var chosen []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, selector := range selectors {
				if selector.Match(n) {
					chosen = append(chosen, n)
					// Do not descend: anything inside is already included.
					return
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	var b strings.Builder
	for _, node := range chosen {
		text := strings.TrimSpace(nodeText(node))
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// nodeText is an element's visible text, with script and style content left
// out — a selector matching a container would otherwise sweep in whatever
// JavaScript happens to live inside it.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" ||
			n.Data == "noscript" || n.Data == "template") {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
			// Block-level elements end a line, so words either side of a
			// boundary do not run together into a token that is neither.
			if child.Type == html.ElementNode && blockLevel[child.Data] {
				b.WriteString("\n")
			}
		}
	}
	walk(n)
	return collapseSpace(b.String())
}

// blockLevel are the elements whose boundaries are worth a line break.
var blockLevel = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "header": true,
	"footer": true, "aside": true, "nav": true, "main": true, "figure": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"table": true, "tr": true, "td": true, "th": true, "thead": true,
	"tbody": true, "blockquote": true, "pre": true, "br": true, "hr": true,
}

// collapseSpace normalises runs of whitespace, keeping paragraph breaks.
func collapseSpace(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.Join(strings.Fields(line), " "); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n")
}
