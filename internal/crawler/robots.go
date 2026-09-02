package crawler

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
)

// robotsCache fetches and remembers one robots.txt per host.
//
// Cached for the life of a run rather than a fixed TTL: a crawl is bounded in
// pages and minutes, and re-fetching robots.txt for every page would be both
// wasteful and, ironically, impolite.
type robotsCache struct {
	http *http.Client

	mu    sync.Mutex
	hosts map[string]*robotstxt.RobotsData
}

func newRobotsCache(client *http.Client) *robotsCache {
	return &robotsCache{http: client, hosts: map[string]*robotstxt.RobotsData{}}
}

// allowed reports whether the crawler may fetch a URL, and any Crawl-delay
// the host asks for.
//
// A robots.txt that cannot be fetched is treated as permissive. That is the
// convention the standard describes — absence of a file means no restrictions
// — and the alternative would let one unreachable file silently halt a crawl
// with no way for the operator to tell the difference between "forbidden" and
// "the server hiccupped".
func (r *robotsCache) allowed(ctx context.Context, target *url.URL) (bool, time.Duration, error) {
	data, err := r.forHost(ctx, target)
	if err != nil || data == nil {
		return true, 0, nil
	}
	group := data.FindGroup(UserAgent)
	if group == nil {
		return true, 0, nil
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	return group.Test(path), group.CrawlDelay, nil
}

func (r *robotsCache) forHost(ctx context.Context, target *url.URL) (*robotstxt.RobotsData, error) {
	key := target.Scheme + "://" + target.Host
	r.mu.Lock()
	data, ok := r.hosts[key]
	r.mu.Unlock()
	if ok {
		return data, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key+"/robots.txt", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := r.http.Do(req)
	if err != nil {
		// Unreachable: remember the absence so one broken host does not cost
		// a request per page.
		r.remember(key, nil)
		return nil, nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// robotstxt.FromResponse handles the status-code conventions: 4xx means
	// full allow, 5xx means full disallow.
	parsed, err := robotstxt.FromResponse(resp)
	if err != nil {
		r.remember(key, nil)
		return nil, nil
	}
	r.remember(key, parsed)
	return parsed, nil
}

func (r *robotsCache) remember(key string, data *robotstxt.RobotsData) {
	r.mu.Lock()
	r.hosts[key] = data
	r.mu.Unlock()
}
