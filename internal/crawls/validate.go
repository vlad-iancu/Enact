package crawls

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"enact/internal/crawler"
)

// Platform ceilings. A crawl's own limits may be lower but never higher: a
// crawl spends the platform's bandwidth, its reputation with the sites it
// visits, and Bedrock embedding cost for every page it stores, so the
// operator's ceiling wins over the author's preference.
const (
	MaxPagesCeiling = 2000
	MaxDepthCeiling = 10
	// JIRAMaxDepthCeiling bounds how far issue relationships may be followed.
	//
	// Tighter than the crawl's own depth ceiling on purpose. A web link is one
	// step to one page; an issue relationship is one step to every subtask,
	// every linked issue and the parent, reciprocally — issue graphs have
	// almost no diameter, so each hop multiplies rather than adds. Four is
	// already an epic, its children, their subtasks and everything those link
	// to, which is more than any single piece of work.
	JIRAMaxDepthCeiling = 4
	MaxDurationCeiling  = 3600 // seconds
	MinIntervalMinutes  = 15
	// MaxExtractionRules and MaxSelectorsPerRule bound what one crawl may
	// carry. Generous for the purpose — a handful of URL shapes per site, a
	// handful of selectors each — and small enough that a rule set cannot
	// become a way to make every page cost arbitrary work.
	MaxExtractionRules  = 20
	MaxSelectorsPerRule = 10
	MaxSelectorLength   = 200
	MaxSeeds            = 20
	MaxNameLength       = 200
	MaxQueryLength      = 1000
)

// Validate checks a crawl's own fields, returning an author-facing message.
//
// It deliberately does NOT check that the knowledge base exists or is empty:
// that needs a call to another service, and mixing "is this well-formed" with
// "does the world agree" makes both harder to test. The handler does the
// second part.
func Validate(c Crawl) (string, bool) {
	if strings.TrimSpace(c.Name) == "" {
		return "name is required", false
	}
	if len(c.Name) > MaxNameLength {
		return fmt.Sprintf("name is longer than %d characters", MaxNameLength), false
	}
	if strings.TrimSpace(c.Query) == "" {
		return "query is required: it is what the crawl searches for", false
	}
	if len(c.Query) > MaxQueryLength {
		return fmt.Sprintf("query is longer than %d characters", MaxQueryLength), false
	}
	if strings.TrimSpace(c.KnowledgeBaseID) == "" {
		return "knowledge_base_id is required", false
	}
	if len(c.SeedURLs) == 0 {
		return "at least one seed URL is required", false
	}
	if len(c.SeedURLs) > MaxSeeds {
		return fmt.Sprintf("at most %d seed URLs are allowed", MaxSeeds), false
	}
	if msg, ok := validateSource(c); !ok {
		return msg, false
	}
	if c.MaxPages < 1 || c.MaxPages > MaxPagesCeiling {
		return fmt.Sprintf("max_pages must be between 1 and %d", MaxPagesCeiling), false
	}
	if c.MaxDepth < 1 || c.MaxDepth > MaxDepthCeiling {
		return fmt.Sprintf("max_depth must be between 1 and %d", MaxDepthCeiling), false
	}
	if c.MaxDurationSec < 1 || c.MaxDurationSec > MaxDurationCeiling {
		return fmt.Sprintf("max_duration_seconds must be between 1 and %d", MaxDurationCeiling), false
	}
	if c.ScoreThreshold < 0 || c.ScoreThreshold > 1 {
		return "score_threshold must be between 0 and 1", false
	}
	if c.Alpha < 0 || c.Alpha > 1 {
		return "alpha must be between 0 and 1", false
	}
	// Zero means unscheduled, which is legal. Anything positive is bounded
	// below, because a crawl every minute is a denial-of-service against the
	// site it points at.
	if msg, ok := validateExtractionRules(c.ExtractionRules); !ok {
		return msg, false
	}
	if msg, ok := validateCredentials(c.Credentials); !ok {
		return msg, false
	}
	if c.IntervalMinutes != 0 && c.IntervalMinutes < MinIntervalMinutes {
		return fmt.Sprintf("interval_minutes must be 0 (manual only) or at least %d", MinIntervalMinutes), false
	}
	for _, domain := range c.AllowedDomains {
		if strings.TrimSpace(domain) == "" {
			return "allowed_domains must not contain empty entries", false
		}
		if strings.ContainsAny(domain, "/:") {
			return fmt.Sprintf("allowed_domains takes bare domains, not URLs: %q", domain), false
		}
	}
	return "", true
}

// validateSeed checks one seed URL.
//
// Only http and https, and only absolute URLs with a host. A seed is the one
// piece of a crawl that a user types freehand and that the platform then
// requests from its own network position, so what it may name is worth being
// strict about — the network-level guard against private addresses is in the
// fetcher, and this is the readable half of the same rule.
func validateSeed(seed string) (string, bool) {
	s := strings.TrimSpace(seed)
	if s == "" {
		return "seed URLs must not be empty", false
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Sprintf("seed URL %q is not a valid URL", seed), false
	}
	// Missing scheme and wrong scheme are different mistakes and deserve
	// different sentences. "/docs" and "not a url" both parse into a bare
	// path, and telling their author to "use http or https" describes a
	// problem they do not have.
	if u.Scheme == "" {
		return fmt.Sprintf("seed URL %q must be absolute, beginning with http:// or https://", seed), false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Sprintf("seed URL %q must use http or https, not %q", seed, u.Scheme), false
	}
	if u.Host == "" {
		return fmt.Sprintf("seed URL %q must be absolute, beginning with http:// or https://", seed), false
	}
	return "", true
}

// ApplyDefaults fills the unset bounds of a new crawl.
//
// AllowedDomains defaults to the registrable domain of every seed, which is
// what makes "focused" mean something by default: without it the first
// outbound link would take the crawl off the site the author pointed at.
func ApplyDefaults(c Crawl) Crawl {
	if c.MaxPages <= 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.MaxDepth <= 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	if c.MaxDurationSec <= 0 {
		c.MaxDurationSec = int(DefaultMaxDuration.Seconds())
	}
	if c.ScoreThreshold <= 0 {
		c.ScoreThreshold = DefaultScoreThreshold
	}
	if c.Alpha <= 0 {
		c.Alpha = 0.7
	}
	if len(c.AllowedDomains) == 0 {
		c.AllowedDomains = defaultDomains(c.SeedURLs)
	}
	c.SeedURLs = normalizeSeeds(c.SeedURLs)
	return c
}

func defaultDomains(seeds []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if domain := crawler.RegistrableDomain(seed); domain != "" && !seen[domain] {
			seen[domain] = true
			out = append(out, domain)
		}
	}
	return out
}

// normalizeSeeds canonicalises seeds so that two spellings of one starting
// point do not become two seeds.
func normalizeSeeds(seeds []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		u, err := url.Parse(strings.TrimSpace(seed))
		if err != nil {
			continue
		}
		normalized := crawler.NormalizeURL(u)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}

// validateExtractionRules checks the rules are usable before a crawl runs on
// them, rather than discovering at three in the morning that a selector never
// compiled and a site has been extracting nothing for a week.
func validateExtractionRules(rules []ExtractionRule) (string, bool) {
	if len(rules) > MaxExtractionRules {
		return fmt.Sprintf("at most %d extraction rules are allowed", MaxExtractionRules), false
	}
	for i, rule := range rules {
		if strings.TrimSpace(rule.URLPattern) == "" {
			return fmt.Sprintf("extraction rule %d: url_pattern is required", i+1), false
		}
		if len(rule.Selectors) == 0 {
			return fmt.Sprintf("extraction rule %d: at least one selector is required", i+1), false
		}
		if len(rule.Selectors) > MaxSelectorsPerRule {
			return fmt.Sprintf("extraction rule %d: at most %d selectors are allowed",
				i+1, MaxSelectorsPerRule), false
		}
		for _, selector := range rule.Selectors {
			if strings.TrimSpace(selector) == "" {
				return fmt.Sprintf("extraction rule %d: a selector is empty", i+1), false
			}
			if len(selector) > MaxSelectorLength {
				return fmt.Sprintf("extraction rule %d: selector is longer than %d characters",
					i+1, MaxSelectorLength), false
			}
			// Compiled here so a typo is a 400 at creation and not silence at
			// run time. The compiler is the same one the crawler uses.
			if err := CheckSelector(selector); err != nil {
				return fmt.Sprintf("extraction rule %d: %q is not a valid CSS selector: %v",
					i+1, selector, err), false
			}
		}
	}
	return "", true
}

// validateSource checks the crawl names a space it can explore, and that its
// seeds are the shape that space expects.
//
// Seed validation moved in here because it is the first thing that differs
// between sources: a URL and an issue key are both "a seed", and only the
// source knows which one it wants.
func validateSource(c Crawl) (string, bool) {
	switch c.Source {
	case "", SourceWeb:
		for _, seed := range c.SeedURLs {
			if msg, ok := validateSeed(seed); !ok {
				return msg, false
			}
		}
		return "", true

	case SourceJIRA:
		if c.JIRA == nil {
			return "jira is required when source is \"jira\": " +
				"give a base_url, an account email and an API token", false
		}
		if strings.TrimSpace(c.JIRA.BaseURL) == "" {
			return "jira.base_url is required, as in https://your-org.atlassian.net", false
		}
		if u, err := url.Parse(strings.TrimSpace(c.JIRA.BaseURL)); err != nil ||
			!u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
			return "jira.base_url must be an absolute https URL", false
		}
		if strings.TrimSpace(c.JIRA.Email) == "" {
			return "jira.email is required: Atlassian authenticates with the " +
				"account email and an API token together", false
		}
		if strings.TrimSpace(c.JIRA.Token) == "" {
			return "jira.token is required", false
		}
		if c.JIRA.MaxDepth < 0 || c.JIRA.MaxDepth > JIRAMaxDepthCeiling {
			return fmt.Sprintf("jira.max_depth must be between 1 and %d (0 uses the default); "+
				"issue relationships fan out reciprocally, so each hop multiplies",
				JIRAMaxDepthCeiling), false
		}
		for _, seed := range c.SeedURLs {
			if !jiraSeed.MatchString(strings.ToUpper(strings.TrimSpace(seed))) &&
				!strings.Contains(seed, "/browse/") {
				return fmt.Sprintf("seed %q is not an issue: expected a key like SCRUM-1, "+
					"or a browse URL containing one", seed), false
			}
		}
		return "", true

	default:
		return fmt.Sprintf("source %q is not one of \"web\" or \"jira\"", c.Source), false
	}
}

// jiraSeed is a bare issue key. A browse URL is checked separately, because a
// URL carrying a key is a perfectly good way to name one.
var jiraSeed = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
