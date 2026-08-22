// Package googleworkspace implements MCP tools over the Google Workspace
// REST APIs — Gmail, Calendar, Drive, Docs, Sheets and Slides.
//
// It holds no credentials and runs no OAuth flow. Every call carries the
// caller's bearer token, which enact minted with the organization's own OAuth
// client and refreshes on its own sweep; this package only spends it. That is
// why there is no `user_google_email` argument anywhere: the token names the
// account, every request goes to /users/me, and a model cannot ask for
// somebody else's mailbox.
package googleworkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API base URLs. Each product is a separate host, and Google is inconsistent
// about which — gmail and docs have their own, calendar and drive live under
// www.googleapis.com.
const (
	gmailBase    = "https://gmail.googleapis.com/gmail/v1/users/me"
	calendarBase = "https://www.googleapis.com/calendar/v3"
	driveBase    = "https://www.googleapis.com/drive/v3"
	docsBase     = "https://docs.googleapis.com/v1"
	sheetsBase   = "https://sheets.googleapis.com/v4"
	slidesBase   = "https://slides.googleapis.com/v1"
)

// requestTimeout bounds one API call. Generous: a Drive export of a large
// document is slow, and the tool loop above has its own limits.
const requestTimeout = 45 * time.Second

// errNoCredential is returned when the request carried no bearer token. It is
// separated from transport failures because it is the one the user can fix,
// and the message has to say so.
var errNoCredential = errors.New(
	"no Google credential was presented: connect a Google account for this agent, then try again")

// client calls the Google APIs as one user.
type client struct {
	token string
	http  *http.Client
}

func newClient(token string) *client {
	return &client{token: token, http: &http.Client{Timeout: requestTimeout}}
}

// apiError is Google's error body, which is worth surfacing verbatim: its
// message is usually the actionable part ("Insufficient Permission", "Invalid
// query"), and inventing our own wording would hide it.
type apiError struct {
	Status  int
	Message string
	Reason  string
}

func (e *apiError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("google api error (HTTP %d, %s): %s", e.Status, e.Reason, e.Message)
	}
	return fmt.Sprintf("google api error (HTTP %d): %s", e.Status, e.Message)
}

// get issues a GET and decodes the JSON body into out.
func (c *client) get(ctx context.Context, endpoint string, query url.Values, out any) error {
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, endpoint, nil, out)
}

// post issues a POST with a JSON body.
func (c *client) post(ctx context.Context, endpoint string, body, out any) error {
	return c.do(ctx, http.MethodPost, endpoint, body, out)
}

// put issues a PUT with a JSON body.
func (c *client) put(ctx context.Context, endpoint string, body, out any) error {
	return c.do(ctx, http.MethodPut, endpoint, body, out)
}

// patch issues a PATCH with a JSON body.
func (c *client) patch(ctx context.Context, endpoint string, body, out any) error {
	return c.do(ctx, http.MethodPatch, endpoint, body, out)
}

// del issues a DELETE.
func (c *client) del(ctx context.Context, endpoint string) error {
	return c.do(ctx, http.MethodDelete, endpoint, nil, nil)
}

// do performs one request. A nil body sends none; a nil out discards the
// response.
func (c *client) do(ctx context.Context, method, endpoint string, body, out any) error {
	if c.token == "" {
		return errNoCredential
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// raw fetches a URL and returns the body as bytes, for the endpoints that do
// not answer JSON (Drive downloads and exports).
func (c *client) raw(ctx context.Context, endpoint string, query url.Values, limit int64) ([]byte, error) {
	if c.token == "" {
		return nil, errNoCredential
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeAPIError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// uploadText writes a file's bytes to an upload endpoint. Drive's media
// upload is not JSON, so it does not go through do().
func (c *client) uploadText(ctx context.Context, endpoint, mimeType, content string) error {
	if c.token == "" {
		return errNoCredential
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, strings.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mimeType)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// decodeAPIError turns Google's error envelope into an *apiError, falling
// back to the raw body when it is not the shape we expect (an HTML error page
// from a wrong path, for instance — which is exactly how a misconfigured
// endpoint presents itself).
func decodeAPIError(resp *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error.Message != "" {
		reason := envelope.Error.Status
		if reason == "" && len(envelope.Error.Errors) > 0 {
			reason = envelope.Error.Errors[0].Reason
		}
		return &apiError{Status: resp.StatusCode, Message: envelope.Error.Message, Reason: reason}
	}
	text := strings.TrimSpace(string(payload))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = http.StatusText(resp.StatusCode)
	}
	return &apiError{Status: resp.StatusCode, Message: text}
}
