package workflows

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

// ClientConfig holds the settings for calling the enact-workflows service.
type ClientConfig struct {
	BaseURL string `env:"WORKFLOW_API_URL, default=http://localhost:8011"`
	// Timeout bounds each call. Generous compared with other services because
	// creating a workflow validates every referenced agent, which is itself a
	// call to another service.
	Timeout time.Duration `env:"WORKFLOW_API_TIMEOUT, default=20s"`
}

// Client is the HTTP wrapper enact-main uses to reach the workflow service.
// It lives in the domain package so callers depend on the domain rather than
// on another service's internals.
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

// SaveRequest is the body of a workflow create or full update.
type SaveRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema declares the trigger payload; enforced when a run starts.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Steps       []Step          `json:"steps"`
}

// TriggerRequest starts an execution. Input is the trigger payload, reachable
// from every step as {{ .Input }}.
type TriggerRequest struct {
	Input json.RawMessage `json:"input,omitempty"`
}

// do issues one JSON request, mirroring the agent client: 404 becomes
// found=false, and 400/403 become typed errors so a refusal reaches the
// browser as a refusal rather than as a broken platform.
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, wantStatus int, out any) (bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, fmt.Errorf("workflows: build %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("workflows: %s %s: %w", method, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case wantStatus:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return false, fmt.Errorf("workflows: decode response: %w", err)
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
		if resp.StatusCode == http.StatusForbidden {
			return false, &requesthelper.ForbiddenError{Message: apiErr.Error}
		}
		return false, &requesthelper.BadRequestError{Message: apiErr.Error}
	default:
		return false, fmt.Errorf("workflows: %s %s: unexpected status %d", method, endpoint, resp.StatusCode)
	}
}

func (c *Client) List(ctx context.Context) ([]Workflow, error) {
	var out struct {
		Workflows []Workflow `json:"workflows"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/workflows", nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Workflows, nil
}

func (c *Client) Get(ctx context.Context, id string) (Workflow, bool, error) {
	var w Workflow
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/workflows/"+url.PathEscape(id), nil, http.StatusOK, &w)
	return w, found, err
}

func (c *Client) Create(ctx context.Context, body SaveRequest) (Workflow, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflows: marshal create request: %w", err)
	}
	var created Workflow
	if _, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/workflows", payload, http.StatusCreated, &created); err != nil {
		return Workflow{}, err
	}
	return created, nil
}

// Update passes the body through verbatim, preserving the callee's
// partial-update semantics end to end.
func (c *Client) Update(ctx context.Context, id string, rawBody json.RawMessage) (Workflow, bool, error) {
	var updated Workflow
	found, err := c.do(ctx, http.MethodPut, c.baseURL+"/v1/workflows/"+url.PathEscape(id), rawBody, http.StatusOK, &updated)
	return updated, found, err
}

func (c *Client) Delete(ctx context.Context, id string) (bool, error) {
	return c.do(ctx, http.MethodDelete, c.baseURL+"/v1/workflows/"+url.PathEscape(id), nil, http.StatusNoContent, nil)
}

// Trigger queues an execution and returns the record as accepted — status
// "queued", no steps run yet.
func (c *Client) Trigger(ctx context.Context, id string, body TriggerRequest) (Execution, bool, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Execution{}, false, fmt.Errorf("workflows: marshal trigger request: %w", err)
	}
	var e Execution
	found, err := c.do(ctx, http.MethodPost,
		c.baseURL+"/v1/workflows/"+url.PathEscape(id)+"/executions", payload, http.StatusAccepted, &e)
	return e, found, err
}

func (c *Client) ListExecutions(ctx context.Context, workflowID string, limit int) ([]Execution, bool, error) {
	endpoint := c.baseURL + "/v1/workflows/" + url.PathEscape(workflowID) + "/executions"
	if limit > 0 {
		endpoint += "?limit=" + strconv.Itoa(limit)
	}
	var out struct {
		Executions []Execution `json:"executions"`
	}
	found, err := c.do(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out)
	return out.Executions, found, err
}

// GetShapes returns the workflow's resolved per-step shapes.
func (c *Client) GetShapes(ctx context.Context, workflowID string) (Shapes, bool, error) {
	var out Shapes
	found, err := c.do(ctx, http.MethodGet,
		c.baseURL+"/v1/workflows/"+url.PathEscape(workflowID)+"/shapes", nil, http.StatusOK, &out)
	return out, found, err
}

func (c *Client) GetExecution(ctx context.Context, id string) (Execution, bool, error) {
	var e Execution
	found, err := c.do(ctx, http.MethodGet, c.baseURL+"/v1/executions/"+url.PathEscape(id), nil, http.StatusOK, &e)
	return e, found, err
}
