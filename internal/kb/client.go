package kb

import (
	"bytes"
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

// NewClient returns a Client for the KB service at cfg.BaseURL. base is the
// innermost RoundTripper — in practice the caller's S2S signing transport —
// wrapped by the tracing transport; nil means plain http.DefaultTransport.
func NewClient(cfg ClientConfig, base http.RoundTripper) *Client {
	httpClient := &http.Client{
		Transport: requesthelper.NewTransport(base),
		Timeout:   cfg.Timeout,
	}
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

// List returns the calling user's knowledge bases.
func (c *Client) List(ctx context.Context) ([]KnowledgeBase, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/knowledge-bases", nil)
	if err != nil {
		return nil, fmt.Errorf("kb: build list request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kb: list: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kb: list: unexpected status %d", resp.StatusCode)
	}
	var body struct {
		KnowledgeBases []KnowledgeBase `json:"knowledge_bases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("kb: decode list response: %w", err)
	}
	return body.KnowledgeBases, nil
}

// Create creates a knowledge base with the given friendly name for the
// calling user; validation failures surface as *requesthelper.BadRequestError.
func (c *Client) Create(ctx context.Context, name string) (KnowledgeBase, error) {
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return KnowledgeBase{}, fmt.Errorf("kb: marshal create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/knowledge-bases", bytes.NewReader(payload))
	if err != nil {
		return KnowledgeBase{}, fmt.Errorf("kb: build create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return KnowledgeBase{}, fmt.Errorf("kb: create: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusCreated:
		var record KnowledgeBase
		if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
			return KnowledgeBase{}, fmt.Errorf("kb: decode create response: %w", err)
		}
		return record, nil
	case http.StatusBadRequest:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = "bad request"
		}
		return KnowledgeBase{}, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return KnowledgeBase{}, fmt.Errorf("kb: create: unexpected status %d", resp.StatusCode)
	}
}

// Update partially updates a knowledge base's own fields (currently the
// name); rawBody passes through verbatim to preserve the callee's partial
// semantics. The boolean reports existence.
func (c *Client) Update(ctx context.Context, id string, rawBody json.RawMessage) (KnowledgeBase, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/v1/knowledge-bases/"+url.PathEscape(id), bytes.NewReader(rawBody))
	if err != nil {
		return KnowledgeBase{}, false, fmt.Errorf("kb: build update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return KnowledgeBase{}, false, fmt.Errorf("kb: update %s: %w", id, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
		var record KnowledgeBase
		if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
			return KnowledgeBase{}, false, fmt.Errorf("kb: decode update response: %w", err)
		}
		return record, true, nil
	case http.StatusNotFound:
		return KnowledgeBase{}, false, nil
	case http.StatusBadRequest:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = "bad request"
		}
		return KnowledgeBase{}, false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return KnowledgeBase{}, false, fmt.Errorf("kb: update %s: unexpected status %d", id, resp.StatusCode)
	}
}

// Delete removes a knowledge base and (server-side) all of its documents.
// The boolean reports existence.
func (c *Client) Delete(ctx context.Context, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/knowledge-bases/"+url.PathEscape(id), nil)
	if err != nil {
		return false, fmt.Errorf("kb: build delete request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("kb: delete %s: %w", id, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("kb: delete %s: unexpected status %d", id, resp.StatusCode)
	}
}

// GetDetail fetches a knowledge base's detail response — the record plus
// its document metadata — verbatim, for relaying to UI clients. The boolean
// reports existence.
func (c *Client) GetDetail(ctx context.Context, id string) (json.RawMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/knowledge-bases/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, false, fmt.Errorf("kb: build detail request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("kb: get detail %s: %w", id, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("kb: read detail response: %w", err)
		}
		return body, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("kb: get detail %s: unexpected status %d", id, resp.StatusCode)
	}
}

// UploadDocuments forwards files to the knowledge base as a multipart
// upload, returning the callee's 202 response body verbatim. The boolean
// reports the knowledge base's existence.
func (c *Client) UploadDocuments(ctx context.Context, id string, files []requesthelper.UploadedFile) (json.RawMessage, bool, error) {
	endpoint := c.baseURL + "/v1/knowledge-bases/" + url.PathEscape(id) + "/documents"
	return requesthelper.PostMultipart(ctx, c.http, "kb", endpoint, files)
}

// DeleteDocument queues the removal of one context document. The boolean
// reports the knowledge base's existence (deletion itself is asynchronous).
func (c *Client) DeleteDocument(ctx context.Context, kbID, docID string) (bool, error) {
	endpoint := c.baseURL + "/v1/knowledge-bases/" + url.PathEscape(kbID) + "/documents/" + url.PathEscape(docID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("kb: build delete document request: %w", err)
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("kb: delete document %s: %w", docID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("kb: delete document %s: unexpected status %d", docID, resp.StatusCode)
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
