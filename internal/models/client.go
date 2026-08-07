package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"enact/internal/identity"
	"enact/internal/requesthelper"
)

// ClientConfig holds the settings for calling the enact-model-management
// service.
type ClientConfig struct {
	// BaseURL is the root URL of the model-management service, without a
	// trailing path. The default matches the port the README assigns it.
	BaseURL string `env:"MODELS_API_URL, default=http://localhost:8081"`

	// Timeout bounds each call to the model-management service end to end.
	Timeout time.Duration `env:"MODELS_API_TIMEOUT, default=10s"`
}

// Client is the HTTP wrapper other services use to fetch the model
// catalogue from the model-management service — the source of truth for
// which models exist.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient returns a Client for the model-management service at
// cfg.BaseURL. base is the innermost RoundTripper — in practice the
// caller's S2S signing transport — wrapped by the tracing transport; nil
// means plain http.DefaultTransport.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	return &Client{
		http: &http.Client{
			Transport: requesthelper.NewTransport(base),
			Timeout:   cfg.Timeout,
		},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
	}
}

// List fetches the model catalogue.
func (c *Client) List(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("models: build list request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: list: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: list: unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Models []Model `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("models: decode list response: %w", err)
	}
	return body.Models, nil
}
