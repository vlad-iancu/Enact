package crawls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling the enact-crawls service.
type ClientConfig struct {
	BaseURL string `env:"CRAWL_API_URL, default=http://localhost:8013"`
	// Timeout bounds each call. Generous because creating a crawl validates
	// the target knowledge base, which is itself a call to another service.
	Timeout time.Duration `env:"CRAWL_API_TIMEOUT, default=20s"`
}

// Client is the HTTP wrapper enact-main uses to reach the crawl service. It
// lives in the domain package so callers depend on the domain rather than on
// another service's internals (ADR-0004).
type Client struct {
	http    *http.Client
	baseURL string
}

func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	return &Client{
		http:    &http.Client{Transport: requesthelper.NewTransport(base), Timeout: cfg.Timeout},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// SaveRequest is the body of a crawl create or update.
type SaveRequest struct {
	Name            string   `json:"name"`
	Query           string   `json:"query"`
	SeedURLs        []string `json:"seed_urls"`
	KnowledgeBaseID string   `json:"knowledge_base_id,omitempty"`
	AllowedDomains  []string `json:"allowed_domains,omitempty"`
	MaxPages        int      `json:"max_pages,omitempty"`
	MaxDepth        int      `json:"max_depth,omitempty"`
	MaxDurationSec  int      `json:"max_duration_seconds,omitempty"`
	ScoreThreshold  float64  `json:"score_threshold,omitempty"`
	Alpha           float64  `json:"alpha,omitempty"`
	IntervalMinutes int      `json:"interval_minutes,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	// The fields below are everything a crawl needs that is not a bound: what
	// space it explores, how to authenticate there, and how to read what it
	// finds. They are carried here so a crawl can be created complete —
	// without them a JIRA crawl had to be made as a web crawl and converted,
	// which meant inventing a seed URL for a crawl that does not use URLs.
	//
	// Pointers where nil and empty differ, matching the API they are sent to:
	// absent leaves the field alone on update, an empty array clears it.
	ExtractionRules *[]ExtractionRule `json:"extraction_rules,omitempty"`
	// Credentials is write-only, like the JIRA token: values are sealed on
	// arrival and never returned.
	Credentials *[]CredentialRule `json:"credentials,omitempty"`
	Source      string            `json:"source,omitempty"`
	JIRA        *JIRAConfig       `json:"jira,omitempty"`
}

func (c *Client) List(ctx context.Context) ([]Crawl, error) {
	var body struct {
		Crawls []Crawl `json:"crawls"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/crawls", nil, http.StatusOK, &body)
	return body.Crawls, err
}

func (c *Client) Get(ctx context.Context, id string) (Crawl, bool, error) {
	var out Crawl
	found, err := c.do(ctx, http.MethodGet, "/v1/crawls/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, found, err
}

func (c *Client) Create(ctx context.Context, body SaveRequest) (Crawl, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Crawl{}, fmt.Errorf("crawls: marshal create request: %w", err)
	}
	var out Crawl
	_, err = c.do(ctx, http.MethodPost, "/v1/crawls", payload, http.StatusCreated, &out)
	return out, err
}

// Update passes the raw body through, preserving the service's partial-update
// semantics end to end.
func (c *Client) Update(ctx context.Context, id string, raw json.RawMessage) (Crawl, bool, error) {
	var out Crawl
	found, err := c.do(ctx, http.MethodPut, "/v1/crawls/"+url.PathEscape(id), raw, http.StatusOK, &out)
	return out, found, err
}

func (c *Client) Delete(ctx context.Context, id string) (bool, error) {
	return c.do(ctx, http.MethodDelete, "/v1/crawls/"+url.PathEscape(id), nil, http.StatusNoContent, nil)
}

func (c *Client) Trigger(ctx context.Context, id string) (Run, bool, error) {
	var out Run
	found, err := c.do(ctx, http.MethodPost, "/v1/crawls/"+url.PathEscape(id)+"/runs", nil, http.StatusAccepted, &out)
	return out, found, err
}

func (c *Client) ListRuns(ctx context.Context, crawlID string, limit int) ([]Run, bool, error) {
	path := "/v1/crawls/" + url.PathEscape(crawlID) + "/runs"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var body struct {
		Runs []Run `json:"runs"`
	}
	found, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &body)
	return body.Runs, found, err
}

func (c *Client) GetRun(ctx context.Context, id string) (Run, bool, error) {
	var out Run
	found, err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, found, err
}

// do performs one call, mapping statuses onto the platform's conventions:
// 404 becomes found=false rather than an error, and a refusal keeps its shape
// so enact-main can relay it unchanged.
func (c *Client) do(ctx context.Context, method, path string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return false, fmt.Errorf("crawls: build %s %s: %w", method, path, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("crawls: %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case wantStatus:
		if out == nil {
			return true, nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return true, fmt.Errorf("crawls: decode %s response: %w", path, err)
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusBadRequest, http.StatusForbidden, http.StatusBadGateway:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = "request refused"
		}
		if resp.StatusCode == http.StatusForbidden {
			return false, &requesthelper.ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("crawls: %s %s: unexpected status %d", method, path, resp.StatusCode)
	}
}
