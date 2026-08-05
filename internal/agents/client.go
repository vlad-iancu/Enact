package agents

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

// ClientConfig holds the settings for calling the enact-agent-management-api
// service.
type ClientConfig struct {
	// BaseURL is the root URL of the agent management service, without a
	// trailing path. The default matches the port the README assigns it.
	BaseURL string `env:"AGENT_API_URL, default=http://localhost:8084"`

	// Timeout bounds each call to the agent service end to end.
	Timeout time.Duration `env:"AGENT_API_TIMEOUT, default=10s"`
}

// Client is the HTTP wrapper other services use to talk to the agent
// management service — the source of truth for agent records. It lives in
// the agents domain package so callers depend on the domain, never on
// another service's internals.
//
// The underlying http.Client comes from requesthelper, so every call
// propagates the caller's trace context and shows up in Tempo as part of
// the originating request's trace.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the agent service at cfg.BaseURL. base is
// the innermost RoundTripper — in practice the caller's S2S signing
// transport — wrapped by the tracing transport; nil means plain
// http.DefaultTransport.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	httpClient := &http.Client{
		Transport: requesthelper.NewTransport(base),
		Timeout:   cfg.Timeout,
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(cfg.BaseURL, "/")}
}

// Get fetches an agent by id from the agent service. The calling user is
// taken from ctx and forwarded as the platform's stub-auth header. The
// boolean reports existence; any transport failure or unexpected status is
// an error.
func (c *Client) Get(ctx context.Context, id string) (Agent, bool, error) {
	endpoint := c.baseURL + "/v1/agents/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Agent{}, false, fmt.Errorf("agents: build get request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return Agent{}, false, fmt.Errorf("agents: get %s: %w", id, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		var record Agent
		if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
			return Agent{}, false, fmt.Errorf("agents: decode get response: %w", err)
		}
		return record, true, nil
	case http.StatusNotFound:
		return Agent{}, false, nil
	default:
		return Agent{}, false, fmt.Errorf("agents: get %s: unexpected status %d", id, resp.StatusCode)
	}
}