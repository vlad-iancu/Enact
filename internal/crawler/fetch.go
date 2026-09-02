// Package crawler is the focused crawler: it fetches pages, scores them for
// relevance to a query, and uses that score to decide where to go next.
//
// "Focused" is the whole point. A general crawler follows every link and
// filters afterwards; this one ranks unvisited links by how likely they are
// to lead somewhere relevant and always takes the most promising first, so a
// bounded budget is spent on the part of the web that matters. The ranking
// comes from internal/wsd — sense-level similarity between the query and the
// page, blended with BM25 over the expanded query.
package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"enact/internal/netguard"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fetch limits. Defaults are conservative: this software makes requests to
// other people's servers, and the cost of being impolite is borne by them.
const (
	// DefaultHostDelay is the minimum gap between two requests to one host.
	DefaultHostDelay = time.Second
	// DefaultConcurrency caps simultaneous fetches across all hosts.
	DefaultConcurrency = 4
	// DefaultMaxPageBytes caps one response body.
	DefaultMaxPageBytes = 4 << 20 // 4 MiB
	// UserAgent identifies the crawler. A crawler that does not say who it is
	// cannot be blocked selectively, so site owners block it broadly.
	UserAgent = "enact-crawler/1.0 (+https://github.com/enact; focused topical crawler)"
)

// Fetch failures a caller distinguishes. Everything else is a transport error
// and is recorded against the page rather than stopping the crawl.
var (
	// ErrBlockedByRobots means robots.txt disallows the path.
	ErrBlockedByRobots = errors.New("crawler: disallowed by robots.txt")
	// ErrNotHTML means the response was not a document worth extracting.
	ErrNotHTML = errors.New("crawler: not an HTML document")
	// ErrTooLarge means the body exceeded the size cap.
	ErrTooLarge = errors.New("crawler: response too large")
	// ErrPrivateAddress means the host resolved to an address inside the
	// deployment's own network. See the dialer for why this matters.
	// The same value the shared guard returns, so errors.Is keeps working
	// after the check moved: a sentinel that merely reads alike would have
	// broken the classification silently.
	ErrPrivateAddress = netguard.ErrPrivateAddress
)

// FetchConfig bounds outbound requests.
type FetchConfig struct {
	HostDelay    time.Duration `env:"CRAWL_HOST_DELAY, default=1s"`
	Concurrency  int           `env:"CRAWL_CONCURRENCY, default=4"`
	MaxPageBytes int64         `env:"CRAWL_MAX_PAGE_BYTES, default=4194304"`
	Timeout      time.Duration `env:"CRAWL_FETCH_TIMEOUT, default=20s"`
	// AllowPrivateAddresses disables the SSRF guard. It exists so the test
	// suite can crawl a local httptest server, and must never be set in a
	// deployment that accepts seed URLs from users.
	AllowPrivateAddresses bool `env:"CRAWL_ALLOW_PRIVATE_ADDRESSES, default=false"`
}

// Fetcher performs polite, bounded HTTP GETs.
//
// Politeness is not optional here. A crawl is triggered by a user but runs on
// the platform's address, so every request a site sees is attributable to the
// deployment rather than to the person who asked — which makes rate limiting,
// robots.txt and an honest User-Agent the platform's responsibility.
type Fetcher struct {
	http         *http.Client
	robots       *robotsCache
	maxPageBytes int64
	hostDelay    time.Duration

	// slots caps concurrent requests across all hosts.
	slots chan struct{}

	// mu and lastSeen are pointers/maps so a credentialed session shares one
	// rate limiter with the fetcher it came from: politeness is a property of
	// the host being crawled, not of which credentials are in play.
	mu       *sync.Mutex
	lastSeen map[string]time.Time
}

// NewFetcher builds a Fetcher.
func NewFetcher(cfg FetchConfig) *Fetcher {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxPageBytes <= 0 {
		cfg.MaxPageBytes = DefaultMaxPageBytes
	}
	if cfg.HostDelay < 0 {
		cfg.HostDelay = DefaultHostDelay
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	transport := &http.Transport{
		DialContext:         guardedDialer(cfg.AllowPrivateAddresses),
		MaxIdleConns:        32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		// Redirects are followed, but each hop is re-checked by the dialer,
		// so a redirect cannot be used to reach a private address.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("crawler: too many redirects")
			}
			return nil
		},
	}
	f := &Fetcher{
		http:         client,
		maxPageBytes: cfg.MaxPageBytes,
		hostDelay:    cfg.HostDelay,
		slots:        make(chan struct{}, cfg.Concurrency),
		mu:           &sync.Mutex{},
		lastSeen:     map[string]time.Time{},
	}
	f.robots = newRobotsCache(client)
	return f
}

// guardedDialer refuses connections to addresses inside the deployment's own
// network.
//
// A crawl's seed URL and every link it follows are untrusted input: they
// arrive from a user, or from a page a user pointed at. Without this, a seed
// of http://169.254.169.254/ reaches the cloud metadata service and a seed of
// http://localhost:9200/ reaches OpenSearch — with the platform's own network
// position, which is exactly the confused-deputy shape of SSRF.
//
// The check is on the RESOLVED address rather than the hostname, and in the
// dialer rather than before the request, because a hostname check is
// defeated by a DNS record that points at 127.0.0.1, and a pre-flight
// resolution is defeated by a name that resolves differently the second time
// (DNS rebinding). By the time the dialer sees it, the address is the one the
// connection will actually use.
// guardedDialer refuses private addresses. The rule lives in internal/netguard
// so every place the platform fetches a user-chosen URL uses the same one.
func guardedDialer(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return netguard.Dialer(allowPrivate)
}

// isPrivate is kept as this package's name for the shared check.
func isPrivate(ip net.IP) bool { return netguard.IsPrivate(ip) }

// Page is a fetched document.
type Page struct {
	URL         string
	FinalURL    string // after redirects
	StatusCode  int
	ContentType string
	Body        []byte
}

// Get fetches one URL, honouring robots.txt, the per-host delay and the size
// cap.
func (f *Fetcher) Get(ctx context.Context, raw string) (Page, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return Page{}, fmt.Errorf("crawler: parse %q: %w", raw, err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return Page{}, fmt.Errorf("crawler: unsupported scheme %q", target.Scheme)
	}

	allowed, crawlDelay, err := f.robots.allowed(ctx, target)
	if err != nil {
		return Page{}, err
	}
	if !allowed {
		return Page{}, fmt.Errorf("%w: %s", ErrBlockedByRobots, target.Path)
	}

	// Concurrency slot first, then the per-host wait, so a host's delay is
	// not spent holding a slot another host could use.
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-ctx.Done():
		return Page{}, ctx.Err()
	}
	if err := f.waitForHost(ctx, target.Host, crawlDelay); err != nil {
		return Page{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")

	resp, err := f.http.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("crawler: get %s: %w", raw, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	page := Page{
		URL:         raw,
		FinalURL:    resp.Request.URL.String(),
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}
	if resp.StatusCode != http.StatusOK {
		return page, fmt.Errorf("crawler: get %s: status %d", raw, resp.StatusCode)
	}
	if !isHTML(page.ContentType) {
		return page, fmt.Errorf("%w: %s", ErrNotHTML, page.ContentType)
	}
	// One byte past the cap, so exceeding it is detectable rather than
	// silently truncating a document into nonsense.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxPageBytes+1))
	if err != nil {
		return page, fmt.Errorf("crawler: read %s: %w", raw, err)
	}
	if int64(len(body)) > f.maxPageBytes {
		return page, fmt.Errorf("%w: over %d bytes", ErrTooLarge, f.maxPageBytes)
	}
	page.Body = body
	return page, nil
}

// waitForHost enforces the politeness delay for one host.
func (f *Fetcher) waitForHost(ctx context.Context, host string, crawlDelay time.Duration) error {
	delay := f.hostDelay
	// robots.txt may ask for more than the configured default. Asking for
	// less is ignored: the site's preference raises politeness, never lowers
	// it below what the operator chose.
	if crawlDelay > delay {
		delay = crawlDelay
	}
	if delay <= 0 {
		return nil
	}
	f.mu.Lock()
	last, seen := f.lastSeen[host]
	now := time.Now()
	var wait time.Duration
	if seen {
		if elapsed := now.Sub(last); elapsed < delay {
			wait = delay - elapsed
		}
	}
	// Reserve the slot before releasing the lock, so two goroutines racing on
	// the same host queue behind each other instead of both going now.
	f.lastSeen[host] = now.Add(wait)
	f.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isHTML(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}

// Session returns a fetcher that presents a crawl's credentials.
//
// It SHARES the per-host delay, the concurrency slots and the robots cache
// with the fetcher it came from — those are promises about how the platform
// treats a site, and they cannot be per-crawl or a site would see one crawl's
// politeness multiplied by however many crawls are running.
//
// What differs is the HTTP client, wrapped so that each redirect hop is
// re-evaluated against the rules. See credentialTransport.
func (f *Fetcher) Session(rules []CredentialRule) *Fetcher {
	if len(rules) == 0 {
		return f
	}
	base := f.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client := &http.Client{
		Transport:     newCredentialTransport(base, rules),
		Timeout:       f.http.Timeout,
		CheckRedirect: f.http.CheckRedirect,
	}
	return &Fetcher{
		http:         client,
		robots:       f.robots,
		maxPageBytes: f.maxPageBytes,
		hostDelay:    f.hostDelay,
		slots:        f.slots,
		mu:           f.mu,
		lastSeen:     f.lastSeen,
	}
}
