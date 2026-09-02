package utils

import (
	"fmt"
	"net/http"
	"strings"
)

// Helpers for the focused-crawl cases.

const CrawlAudience = "enact-crawls"

type CrawlDTO struct {
	ID              string   `json:"id"`
	UserID          string   `json:"user_id"`
	Name            string   `json:"name"`
	Query           string   `json:"query"`
	SeedURLs        []string `json:"seed_urls"`
	KnowledgeBaseID string   `json:"knowledge_base_id"`
	AllowedDomains  []string `json:"allowed_domains"`
	MaxPages        int      `json:"max_pages"`
	MaxDepth        int      `json:"max_depth"`
	ScoreThreshold  float64  `json:"score_threshold"`
	Alpha           float64  `json:"alpha"`
	IntervalMinutes int      `json:"interval_minutes"`
	NextRunAt       string   `json:"next_run_at"`
	Enabled         bool     `json:"enabled"`
	Error           string   `json:"error"`
}

type CrawlRunDTO struct {
	ID         string `json:"id"`
	CrawlID    string `json:"crawl_id"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason"`
	Error      string `json:"error"`
	Analysis   struct {
		Query     string `json:"query"`
		Terms     []any  `json:"terms"`
		Expansion []any  `json:"expansion"`
	} `json:"analysis"`
	Graph struct {
		Nodes []any `json:"nodes"`
		Edges []any `json:"edges"`
	} `json:"graph"`
	APIErr string `json:"error_message"`
}

func (t *T) CrawlURL(path string) string { return t.Env.CrawlAPIURL + path }

// CreateCrawlBody builds a create request against a knowledge base. The
// bounds are deliberately tiny: cases that do not run the crawl still have to
// pass validation, and one that does must not wander.
func CreateCrawlBody(name, query, seed, kbID string) string {
	return fmt.Sprintf(`{"name":%q,"query":%q,"seed_urls":[%q],"knowledge_base_id":%q,
		"max_pages":2,"max_depth":1,"max_duration_seconds":30,"score_threshold":0.2,
		"interval_minutes":0}`, name, query, seed, kbID)
}

// CreateCrawl creates a crawl against an empty retrieval KB; pair every call
// with a DeleteCrawl in TearDown.
func (t *T) CreateCrawl(body string) CrawlDTO {
	var out CrawlDTO
	status := t.DoJSON("enact-tests", CrawlAudience, http.MethodPost,
		t.CrawlURL("/v1/crawls"), strings.NewReader(body), &out)
	if status != http.StatusCreated {
		t.Fatalf("create crawl: got HTTP %d (%s), want 201", status, out.Error)
	}
	if out.ID == "" {
		t.Fatalf("create crawl: response has no id")
	}
	return out
}

// DeleteCrawl removes a crawl. TearDown-tolerant: an empty id is a no-op and
// 404 is accepted.
func (t *T) DeleteCrawl(id string) {
	if id == "" {
		return
	}
	status := t.DoJSON("enact-tests", CrawlAudience, http.MethodDelete,
		t.CrawlURL("/v1/crawls/"+id), nil, nil)
	if status != http.StatusNoContent && status != http.StatusNotFound {
		t.Errorf("delete crawl %s: got HTTP %d, want 204", id, status)
	}
}

// ListCrawlIDs returns the ids of the caller's crawls.
func (t *T) ListCrawlIDs() map[string]bool {
	var out struct {
		Crawls []CrawlDTO `json:"crawls"`
		Error  string     `json:"error"`
	}
	status := t.DoJSON("enact-tests", CrawlAudience, http.MethodGet,
		t.CrawlURL("/v1/crawls"), nil, &out)
	if status != http.StatusOK {
		t.Fatalf("list crawls: got HTTP %d (%s), want 200", status, out.Error)
	}
	ids := make(map[string]bool, len(out.Crawls))
	for _, c := range out.Crawls {
		ids[c.ID] = true
	}
	return ids
}
