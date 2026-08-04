// Package tika extracts plain text from uploaded documents by delegating to a
// self-hosted Apache Tika server (tika-server). It is a thin abstraction over
// the google/go-tika client: callers hand over raw file bytes and get back the
// parsed text, without depending on go-tika's types directly. Tika auto-detects
// the format from the bytes, so a single call handles PDFs, Office documents,
// HTML, plain text, and the many other formats Tika understands.
package tika

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gotika "github.com/google/go-tika/tika"

	"enact/internal/requesthelper"
)

// Config holds the environment-driven settings for reaching the Tika server.
type Config struct {
	URL     string        `env:"TIKA_URL, default=http://localhost:9998"`
	Timeout time.Duration `env:"TIKA_TIMEOUT, default=120s"`
}

// Client extracts text from documents via a Tika server.
type Client struct {
	tika *gotika.Client
}

// NewClient returns a Client targeting the Tika server at cfg.URL.
func NewClient(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	// Wrap the client so outbound Tika calls carry the caller's trace context,
	// keeping document extraction on the same trace as the request that
	// triggered it.
	httpClient := requesthelper.WrapClient(&http.Client{Timeout: timeout})
	return &Client{
		tika: gotika.NewClient(httpClient, strings.TrimRight(cfg.URL, "/")),
	}
}

// Extract returns the plain-text content Tika parses from data. filename is a
// detection hint only (passed via Content-Disposition); Tika still auto-detects
// the format from the bytes, so an empty or wrong filename is harmless.
func (c *Client) Extract(ctx context.Context, filename string, data []byte) (string, error) {
	header := http.Header{}
	// Ask for plain text rather than Tika's default XHTML rendering.
	header.Set("Accept", "text/plain")
	if filename != "" {
		header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}

	text, err := c.tika.ParseWithHeader(ctx, bytes.NewReader(data), header)
	if err != nil {
		return "", fmt.Errorf("tika: parse document: %w", err)
	}
	return text, nil
}
