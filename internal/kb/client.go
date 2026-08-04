package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling the enact-kb-api service.
type ClientConfig struct {
	// BaseURL is the root URL of the enact-kb-api service, without a
	// trailing path. The default matches the port the README assigns it.
	BaseURL string `env:"KB_API_URL, default=http://localhost:8082"`

	// Timeout bounds each call to the KB service end to end.
	Timeout time.Duration `env:"KB_API_TIMEOUT, default=10s"`
}

// Client is the HTTP wrapper other services use to talk to enact-kb-api.
// It lives here in the kb domain package (not in enactkbapi) so callers
// depend on the domain, never on another service's internals.
//
// The underlying http.Client comes from requesthelper, so every call
// propagates the caller's trace context and shows up in Tempo as part of
// the originating request's trace.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the KB service at cfg.BaseURL.
func NewClient(cfg ClientConfig) *Client {
	httpClient := requesthelper.Client()
	httpClient.Timeout = cfg.Timeout
	return &Client{http: httpClient, baseURL: strings.TrimRight(cfg.BaseURL, "/")}
}

// Get fetches a knowledge base by id from the KB service. The calling user
// is taken from ctx (put there by identity.Filter) and forwarded as the
// platform's stub-auth header. The boolean reports existence; any transport
// failure or unexpected status is an error.
func (c *Client) Get(ctx context.Context, id string) (KnowledgeBase, bool, error) {
	endpoint := c.baseURL + "/v1/knowledge-bases/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return KnowledgeBase{}, false, fmt.Errorf("kb: build get request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return KnowledgeBase{}, false, fmt.Errorf("kb: get %s: %w", id, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		var record KnowledgeBase
		if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
			return KnowledgeBase{}, false, fmt.Errorf("kb: decode get response: %w", err)
		}
		return record, true, nil
	case http.StatusNotFound:
		return KnowledgeBase{}, false, nil
	default:
		return KnowledgeBase{}, false, fmt.Errorf("kb: get %s: unexpected status %d", id, resp.StatusCode)
	}
}

// ListDocuments fetches every document of a knowledge base, extracted text
// included, from the KB service. The calling user is taken from ctx; callers
// acting on behalf of another user (e.g. inference loading documents for an
// agent's owner) impersonate them via identity.WithUserID first. The boolean
// reports whether the knowledge base exists.
func (c *Client) ListDocuments(ctx context.Context, kbID string) ([]Document, bool, error) {
	endpoint := c.baseURL + "/v1/knowledge-bases/" + url.PathEscape(kbID) + "/documents"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("kb: build list documents request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("kb: list documents of %s: %w", kbID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			KBID      string     `json:"kb_id"`
			Documents []Document `json:"documents"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, false, fmt.Errorf("kb: decode list documents response: %w", err)
		}
		for i := range body.Documents {
			body.Documents[i].KBID = body.KBID
		}
		return body.Documents, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("kb: list documents of %s: unexpected status %d", kbID, resp.StatusCode)
	}
}