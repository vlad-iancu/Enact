// Package opensearch provides a thin, JSON-friendly wrapper around the
// OpenSearch Go client (opensearch-go/v2). It owns connection setup and
// exposes the small set of index/document operations the enact services
// need: ensuring indices exist, indexing/getting/deleting documents,
// running searches, and deleting-by-query.
//
// Bodies are passed and returned as raw JSON ([]byte) to keep the wrapper
// generic; the domain-specific shapes live in the store package.
package opensearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	opensearchgo "github.com/opensearch-project/opensearch-go/v2"
	"github.com/opensearch-project/opensearch-go/v2/opensearchapi"
)

// Config holds the environment-driven settings for connecting to OpenSearch.
type Config struct {
	Addresses          []string `env:"OPENSEARCH_ADDRESSES, default=https://localhost:9200"`
	Username           string   `env:"OPENSEARCH_USERNAME, default=admin"`
	Password           string   `env:"OPENSEARCH_PASSWORD, default=9zT!mPq4#bRk7Fx2"`
	InsecureSkipVerify bool     `env:"OPENSEARCH_INSECURE_SKIP_VERIFY, default=true"`
}

// Client wraps an *opensearchgo.Client with helper methods.
type Client struct {
	api *opensearchgo.Client
}

// NewClient builds an OpenSearch client from cfg. The single-node OpenSearch
// dev image ships with a self-signed certificate and HTTP basic auth, hence
// the InsecureSkipVerify default.
func NewClient(cfg Config) (*Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}, //nolint:gosec // dev cluster uses a self-signed cert
	}
	api, err := opensearchgo.NewClient(opensearchgo.Config{
		Addresses: cfg.Addresses,
		Username:  cfg.Username,
		Password:  cfg.Password,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("opensearch: new client: %w", err)
	}
	return &Client{api: api}, nil
}

// IndexExists reports whether the named index exists.
func (c *Client) IndexExists(ctx context.Context, index string) (bool, error) {
	req := opensearchapi.IndicesExistsRequest{Index: []string{index}}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return false, fmt.Errorf("opensearch: index exists %q: %w", index, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.IsError() {
		return false, fmt.Errorf("opensearch: index exists %q: %s", index, res.String())
	}
	return true, nil
}

// CreateIndex creates index with the supplied mapping/settings JSON body. If
// the index already exists it is left untouched.
func (c *Client) CreateIndex(ctx context.Context, index string, body []byte) error {
	exists, err := c.IndexExists(ctx, index)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	req := opensearchapi.IndicesCreateRequest{Index: index, Body: bytes.NewReader(body)}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return fmt.Errorf("opensearch: create index %q: %w", index, err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return fmt.Errorf("opensearch: create index %q: %s", index, res.String())
	}
	return nil
}

// IndexDoc indexes (creates or replaces) a document by id, refreshing the
// index so the write is immediately searchable.
func (c *Client) IndexDoc(ctx context.Context, index, id string, body []byte) error {
	req := opensearchapi.IndexRequest{
		Index:      index,
		DocumentID: id,
		Body:       bytes.NewReader(body),
		Refresh:    "true",
	}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return fmt.Errorf("opensearch: index doc %s/%s: %w", index, id, err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return fmt.Errorf("opensearch: index doc %s/%s: %s", index, id, res.String())
	}
	return nil
}

// GetSource fetches a document's _source by id. The boolean reports whether
// the document was found; when false, out is left untouched.
func (c *Client) GetSource(ctx context.Context, index, id string, out any) (bool, error) {
	req := opensearchapi.GetRequest{Index: index, DocumentID: id}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return false, fmt.Errorf("opensearch: get %s/%s: %w", index, id, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.IsError() {
		return false, fmt.Errorf("opensearch: get %s/%s: %s", index, id, res.String())
	}
	var envelope struct {
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return false, fmt.Errorf("opensearch: decode get %s/%s: %w", index, id, err)
	}
	if !envelope.Found {
		return false, nil
	}
	if err := json.Unmarshal(envelope.Source, out); err != nil {
		return false, fmt.Errorf("opensearch: unmarshal source %s/%s: %w", index, id, err)
	}
	return true, nil
}

// DeleteDoc deletes a document by id. A missing document is not an error.
func (c *Client) DeleteDoc(ctx context.Context, index, id string) error {
	req := opensearchapi.DeleteRequest{Index: index, DocumentID: id, Refresh: "true"}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return fmt.Errorf("opensearch: delete %s/%s: %w", index, id, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil
	}
	if res.IsError() {
		return fmt.Errorf("opensearch: delete %s/%s: %s", index, id, res.String())
	}
	return nil
}

// Search runs a query body against index and returns the parsed hits. Each
// hit's _source is returned as raw JSON for the caller to unmarshal.
func (c *Client) Search(ctx context.Context, index string, body []byte) ([]Hit, error) {
	req := opensearchapi.SearchRequest{Index: []string{index}, Body: bytes.NewReader(body)}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return nil, fmt.Errorf("opensearch: search %q: %w", index, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.IsError() {
		return nil, fmt.Errorf("opensearch: search %q: %s", index, res.String())
	}
	var parsed struct {
		Hits struct {
			Hits []Hit `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("opensearch: decode search %q: %w", index, err)
	}
	return parsed.Hits.Hits, nil
}

// Hit is a single search hit.
type Hit struct {
	ID     string          `json:"_id"`
	Score  float64         `json:"_score"`
	Source json.RawMessage `json:"_source"`
}

// SearchResult carries a search's hits plus its raw aggregations.
type SearchResult struct {
	Hits []Hit
	// Total is the number of documents matching the query across all
	// pages (hits.total.value), for paginated listings. Accurate up to
	// 10k unless the query sets track_total_hits.
	Total int
	// Aggregations is the response's "aggregations" object verbatim (nil
	// when the query requested none); callers unmarshal into their own
	// typed structs.
	Aggregations json.RawMessage
}

// SearchWithAggregations runs a query body against index and returns hits
// and aggregations. Search (above) discards aggregations; use this for
// queries whose payload is the aggregation result (e.g. size:0 + terms).
func (c *Client) SearchWithAggregations(ctx context.Context, index string, body []byte) (SearchResult, error) {
	req := opensearchapi.SearchRequest{Index: []string{index}, Body: bytes.NewReader(body)}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return SearchResult{}, fmt.Errorf("opensearch: search %q: %w", index, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return SearchResult{}, nil
	}
	if res.IsError() {
		return SearchResult{}, fmt.Errorf("opensearch: search %q: %s", index, res.String())
	}
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []Hit `json:"hits"`
		} `json:"hits"`
		Aggregations json.RawMessage `json:"aggregations"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return SearchResult{}, fmt.Errorf("opensearch: decode search %q: %w", index, err)
	}
	return SearchResult{Hits: parsed.Hits.Hits, Total: parsed.Hits.Total.Value, Aggregations: parsed.Aggregations}, nil
}

// DeleteByQuery deletes every document in index matching the query body.
func (c *Client) DeleteByQuery(ctx context.Context, index string, body []byte) error {
	refresh := true
	req := opensearchapi.DeleteByQueryRequest{
		Index:   []string{index},
		Body:    bytes.NewReader(body),
		Refresh: &refresh,
	}
	res, err := req.Do(ctx, c.api)
	if err != nil {
		return fmt.Errorf("opensearch: delete by query %q: %w", index, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil
	}
	if res.IsError() {
		return fmt.Errorf("opensearch: delete by query %q: %s", index, res.String())
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}
