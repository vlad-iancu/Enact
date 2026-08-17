package requesthelper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"enact/internal/identity"
)

// BadRequestError carries a downstream 400's message so a calling service
// (the UI backend) can relay validation feedback verbatim instead of
// masking it as an internal error. Domain clients return it with
// errors.As-compatible semantics.
type BadRequestError struct{ Message string }

func (e *BadRequestError) Error() string { return e.Message }

// ForbiddenError is a callee's 403: the caller is known and is not permitted.
// Kept distinct from BadRequestError and from a transport failure, because
// the three call for different answers — fix your request, get a role, or
// wait for the service to come back.
type ForbiddenError struct{ Message string }

func (e *ForbiddenError) Error() string { return e.Message }

// PostMultipart builds a multipart/form-data body (one "file" part per
// file) and POSTs it with the caller's identity, returning the callee's 202
// response body verbatim. 404 maps to found=false; 400 to *BadRequestError.
// domain prefixes error messages ("agents", "kb").
func PostMultipart(ctx context.Context, client *http.Client, domain, endpoint string, files []UploadedFile) (json.RawMessage, bool, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, f := range files {
		part, err := writer.CreateFormFile("file", f.Filename)
		if err != nil {
			return nil, false, fmt.Errorf("%s: build multipart: %w", domain, err)
		}
		if _, err := part.Write(f.Content); err != nil {
			return nil, false, fmt.Errorf("%s: build multipart: %w", domain, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, false, fmt.Errorf("%s: build multipart: %w", domain, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, false, fmt.Errorf("%s: build upload request: %w", domain, err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(identity.Header, identity.FromContext(ctx))
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%s: upload: %w", domain, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusAccepted:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("%s: read upload response: %w", domain, err)
		}
		return body, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	case http.StatusBadRequest:
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = "bad request"
		}
		return nil, false, &BadRequestError{Message: apiErr.Error}
	default:
		return nil, false, fmt.Errorf("%s: upload: unexpected status %d", domain, resp.StatusCode)
	}
}
