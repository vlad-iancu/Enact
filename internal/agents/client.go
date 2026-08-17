package agents

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

// CreateAgentRequest is the body of an agent creation.
type CreateAgentRequest struct {
	Name             string   `json:"name"`
	Model            string   `json:"model"`
	SystemPrompt     string   `json:"system_prompt"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	Tools            []string `json:"tools,omitempty"`
}

// do issues one JSON request and decodes the response. It maps 404 to
// found=false and 400 to *requesthelper.BadRequestError; any other
// unexpected status is a plain error.
func (c *Client) do(ctx context.Context, method, url string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return false, fmt.Errorf("agents: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("agents: %s %s: %w", method, url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case wantStatus:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return false, fmt.Errorf("agents: decode response: %w", err)
			}
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusBadRequest, http.StatusForbidden:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = "bad request"
		}
		// A refusal must reach the caller AS a refusal. Folding it into the
		// generic branch would turn "you may not" into "something broke",
		// and the person who needs a role would go looking for an outage.
		if resp.StatusCode == http.StatusForbidden {
			return false, &requesthelper.ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("agents: %s %s: unexpected status %d", method, url, resp.StatusCode)
	}
}

// List returns the calling user's agents.
func (c *Client) List(ctx context.Context) ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/agents", nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// Create creates an agent; validation failures surface as *BadRequestError.
func (c *Client) Create(ctx context.Context, body CreateAgentRequest) (Agent, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Agent{}, fmt.Errorf("agents: marshal create request: %w", err)
	}
	var created Agent
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/agents", payload, http.StatusCreated, &created); err != nil {
		return Agent{}, err
	}
	return created, nil
}

// Update partially updates an agent. rawBody is passed through verbatim so
// the callee's partial-update semantics (absent field = unchanged) are
// preserved end to end. The boolean reports existence.
func (c *Client) Update(ctx context.Context, id string, rawBody json.RawMessage) (Agent, bool, error) {
	var updated Agent
	found, err := c.do(ctx, http.MethodPut, c.baseURL+"/v1/agents/"+url.PathEscape(id), rawBody, http.StatusOK, &updated)
	if err != nil {
		return Agent{}, false, err
	}
	return updated, found, nil
}

// Delete removes an agent (and, server-side, its RAG collection). The
// boolean reports existence.
func (c *Client) Delete(ctx context.Context, id string) (bool, error) {
	return c.do(ctx, http.MethodDelete, c.baseURL+"/v1/agents/"+url.PathEscape(id), nil, http.StatusNoContent, nil)
}

// ListRAGDocuments returns the distinct documents of the agent's RAG
// collection. The boolean reports the agent's existence.
func (c *Client) ListRAGDocuments(ctx context.Context, id string) ([]RAGDocument, bool, error) {
	var out struct {
		Documents []RAGDocument `json:"documents"`
	}
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/agents/"+url.PathEscape(id)+"/rag/documents", nil, http.StatusOK, &out)
	if err != nil {
		return nil, false, err
	}
	return out.Documents, found, nil
}

// DeleteRAGDocument queues the removal of one RAG document. The boolean
// reports the agent's existence (deletion itself is asynchronous).
func (c *Client) DeleteRAGDocument(ctx context.Context, agentID, docID string) (bool, error) {
	endpoint := c.baseURL + "/v1/agents/" + url.PathEscape(agentID) + "/rag/documents/" + url.PathEscape(docID)
	return c.do(ctx, http.MethodDelete, endpoint, nil, http.StatusAccepted, nil)
}

// UploadRAGDocuments forwards files to the agent's RAG collection as a
// multipart upload. It returns the callee's 202 response body verbatim; the
// boolean reports the agent's existence.
func (c *Client) UploadRAGDocuments(ctx context.Context, id string, files []requesthelper.UploadedFile) (json.RawMessage, bool, error) {
	endpoint := c.baseURL + "/v1/agents/" + url.PathEscape(id) + "/rag/documents"
	return requesthelper.PostMultipart(ctx, c.http, "agents", endpoint, files)
}
