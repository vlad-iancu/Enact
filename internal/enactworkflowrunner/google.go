package enactworkflowrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"enact/internal/extidentities"
	"enact/internal/files"
	"enact/internal/logging"
	"enact/internal/workflows"
)

// Google's endpoints. Written out here rather than imported from the built-in
// MCP server: that lives in another service, and services do not import each
// other (ADR-0009). Two REST calls do not justify a shared package.
const (
	driveBase  = "https://www.googleapis.com/drive/v3"
	docsBase   = "https://docs.googleapis.com/v1"
	sheetsBase = "https://sheets.googleapis.com/v4"
	slidesBase = "https://slides.googleapis.com/v1"
)

// googleResult is what a Google step returns, matching GoogleOutputSchema.
type googleResult struct {
	File         *files.File `json:"file,omitempty"`
	DocumentID   string      `json:"document_id"`
	Format       string      `json:"format,omitempty"`
	Title        string      `json:"title,omitempty"`
	URL          string      `json:"url,omitempty"`
	UpdatedRange string      `json:"updated_range,omitempty"`
	AppendedRows int         `json:"appended_rows,omitempty"`
}

// runGoogleDocs executes one Google Docs step.
//
// The credential is fetched HERE, at the moment the step runs, as the person
// who triggered the workflow — never stored on the workflow and never carried
// on the message. Two people running the same workflow therefore reach their
// own Drive, and nothing about the definition has to change for that to be
// true.
func (r *Runner) runGoogleStep(ctx context.Context, logger *logging.Logger,
	step workflows.Step, stepCtx workflows.Context, execution workflows.Execution) (json.RawMessage, error) {

	// Templates are rendered BEFORE the credential is fetched. Rendering is
	// local and free; fetching is a call to another service. A broken template
	// should fail on its own terms rather than after a pointless round trip —
	// and the author should be told about their template, not their account.
	rendered, err := renderGoogleFields(step, stepCtx)
	if err != nil {
		return nil, err
	}

	if r.identities == nil {
		return nil, fmt.Errorf("this step needs a connected account, but no identities service is configured on the runner")
	}
	credential, found, err := r.identities.Credentials(ctx, step.Provider, nil)
	if err != nil {
		return nil, fmt.Errorf("could not fetch a credential for provider %q: %w", step.Provider, err)
	}
	if !found || credential.Credentials == "" {
		// The person, not the workflow, is what is missing — so the message
		// says what they have to do rather than describing a fault.
		return nil, fmt.Errorf(
			"you have not connected an account for provider %q; connect it and run this again", step.Provider)
	}

	switch step.Operation {
	case workflows.GoogleOperationExport:
		return r.exportGoogleFile(ctx, logger, step, rendered, execution, credential)
	case workflows.GoogleOperationCreate:
		return r.createGoogleFile(ctx, logger, step, rendered, credential)
	case workflows.GoogleOperationAppend:
		return r.appendRows(ctx, logger, step, rendered, credential)
	}
	return nil, fmt.Errorf("unknown operation %q", step.Operation)
}

// exportGoogleFile fetches a document, spreadsheet or deck and stores it as a
// file for later steps. Drive exports all three the same way; only the format
// table differs, and that lives with the step type.
func (r *Runner) exportGoogleFile(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, execution workflows.Execution,
	credential extidentities.Credential) (json.RawMessage, error) {

	if r.files == nil {
		return nil, fmt.Errorf("this step produces a file, but no file store is configured on the runner")
	}
	documentID := rendered.documentID
	if documentID == "" {
		return nil, fmt.Errorf("document_id rendered empty; there is nothing to export")
	}
	format := step.Format
	if format == "" {
		format = workflows.DefaultGoogleExportFormat
	}
	spec, known := workflows.GoogleExportFormat(step.Type, format)
	if !known {
		return nil, fmt.Errorf("unknown export format %q; expected one of %s",
			format, workflows.GoogleExportFormatNames(step.Type))
	}

	// The document's own name, so the stored file is called something a person
	// recognises. A failure here is not fatal — the export is what matters, and
	// a fallback name still carries the extension the file's type is read from.
	name := documentID
	var meta struct {
		Name string `json:"name"`
	}
	if err := r.googleJSON(ctx, credential, http.MethodGet,
		driveBase+"/files/"+url.PathEscape(documentID)+"?fields=name", nil, &meta); err != nil {
		logger.Warn("could not read the document's name; falling back to its id", "err", err)
	} else if meta.Name != "" {
		name = meta.Name
	}

	query := url.Values{}
	query.Set("mimeType", spec.MimeType)
	body, err := r.googleStream(ctx, credential,
		driveBase+"/files/"+url.PathEscape(documentID)+"/export?"+query.Encode())
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	// Streamed into the store rather than buffered: an exported document is
	// arbitrarily large and nothing here has a reason to hold a whole one.
	stored, err := r.files.Put(ctx,
		files.InExecution(execution.WorkflowID, execution.ID), body,
		files.Metadata{Name: withExtension(name, spec.Extension), MimeType: spec.MimeType})
	if err != nil {
		return nil, fmt.Errorf("could not store the exported document: %w", err)
	}
	logger.Info("exported a google file", "step", step.Name, "type", step.Type, "document_id", documentID,
		"format", format, "bytes", stored.Size, "ref", stored.Ref, "attachable", spec.Attachable)

	return json.Marshal(googleResult{File: &stored, DocumentID: documentID, Format: format})
}

// createGoogleFile makes a new document, spreadsheet or presentation.
//
// The three APIs differ enough that this is a switch rather than a shared
// call: a document takes its text in a follow-up batchUpdate, a spreadsheet
// takes its cells in the creation request, and a deck takes nothing at all.
func (r *Runner) createGoogleFile(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, credential extidentities.Credential) (json.RawMessage, error) {

	switch step.Type {
	case workflows.StepTypeGoogleSheets:
		return r.createSpreadsheet(ctx, logger, step, rendered, credential)
	case workflows.StepTypeGoogleSlides:
		return r.createPresentation(ctx, logger, step, rendered, credential)
	default:
		return r.createDocument(ctx, logger, step, rendered, credential)
	}
}

// createDocument makes a Google Doc and writes its body.
func (r *Runner) createDocument(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, credential extidentities.Credential) (json.RawMessage, error) {

	var created struct {
		DocumentID string `json:"documentId"`
	}
	if err := r.googleJSON(ctx, credential, http.MethodPost, docsBase+"/documents",
		map[string]any{"title": rendered.title}, &created); err != nil {
		return nil, fmt.Errorf("could not create the document: %w", err)
	}

	if rendered.body != "" {
		// Index 1 is the start of the body: index 0 sits before the document's
		// first section, which the API refuses to write at.
		update := map[string]any{"requests": []any{
			map[string]any{"insertText": map[string]any{
				"location": map[string]any{"index": 1},
				"text":     rendered.body,
			}},
		}}
		if err := r.googleJSON(ctx, credential, http.MethodPost,
			docsBase+"/documents/"+url.PathEscape(created.DocumentID)+":batchUpdate", update, nil); err != nil {
			// The document exists but is empty. Reported as a failure, because
			// a later step told this succeeded would carry on with a document
			// that is not what was asked for.
			return nil, fmt.Errorf("created document %s but could not write its contents: %w", created.DocumentID, err)
		}
	}
	logger.Info("created a google doc", "step", step.Name, "document_id", created.DocumentID,
		"title_chars", len(rendered.title), "body_chars", len(rendered.body))
	return json.Marshal(googleResult{
		DocumentID: created.DocumentID,
		Title:      rendered.title,
		URL:        "https://docs.google.com/document/d/" + created.DocumentID + "/edit",
	})
}

// createSpreadsheet makes a Google Sheet, with its rows if any were given.
func (r *Runner) createSpreadsheet(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, credential extidentities.Credential) (json.RawMessage, error) {

	body := map[string]any{"properties": map[string]any{"title": rendered.title}}
	// The cells go in the creation request rather than a follow-up write: one
	// call means there is no state where the spreadsheet exists but is empty.
	if len(rendered.rows) > 0 {
		body["sheets"] = []any{map[string]any{
			"data": []any{map[string]any{"startRow": 0, "startColumn": 0, "rowData": rowData(rendered.rows)}},
		}}
	}
	var created struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		SpreadsheetURL string `json:"spreadsheetUrl"`
	}
	if err := r.googleJSON(ctx, credential, http.MethodPost, sheetsBase+"/spreadsheets", body, &created); err != nil {
		return nil, fmt.Errorf("could not create the spreadsheet: %w", err)
	}
	logger.Info("created a google spreadsheet", "step", step.Name,
		"document_id", created.SpreadsheetID, "rows", len(rendered.rows))
	return json.Marshal(googleResult{
		DocumentID: created.SpreadsheetID,
		Title:      rendered.title,
		URL: firstNonEmpty(created.SpreadsheetURL,
			"https://docs.google.com/spreadsheets/d/"+created.SpreadsheetID+"/edit"),
	})
}

// createPresentation makes an empty Google Slides deck.
//
// Empty on purpose: putting content in would mean choosing a slide layout and
// where text sits on it, which is a design decision this step has no business
// making. Validation refuses a body rather than quietly ignoring one.
func (r *Runner) createPresentation(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, credential extidentities.Credential) (json.RawMessage, error) {

	var created struct {
		PresentationID string `json:"presentationId"`
	}
	if err := r.googleJSON(ctx, credential, http.MethodPost, slidesBase+"/presentations",
		map[string]any{"title": rendered.title}, &created); err != nil {
		return nil, fmt.Errorf("could not create the presentation: %w", err)
	}
	logger.Info("created a google presentation", "step", step.Name, "document_id", created.PresentationID)
	return json.Marshal(googleResult{
		DocumentID: created.PresentationID,
		Title:      rendered.title,
		URL:        "https://docs.google.com/presentation/d/" + created.PresentationID + "/edit",
	})
}

// appendRows adds rows after the last row of a spreadsheet range.
//
// USER_ENTERED so "=SUM(A1:A9)" becomes a formula and "5" a number, which is
// what somebody writing a spreadsheet means. RAW would store both as text.
func (r *Runner) appendRows(ctx context.Context, logger *logging.Logger,
	step workflows.Step, rendered googleFields, credential extidentities.Credential) (json.RawMessage, error) {

	if len(rendered.rows) == 0 {
		return nil, fmt.Errorf("rows rendered empty; there is nothing to append")
	}
	target := rendered.rangeA1
	if target == "" {
		// The whole of the first sheet, which is what "log this somewhere"
		// means when nobody said where.
		target = "A:Z"
	}
	endpoint := sheetsBase + "/spreadsheets/" + url.PathEscape(rendered.documentID) +
		"/values/" + url.PathEscape(target) + ":append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS"

	var out struct {
		Updates struct {
			UpdatedRange string `json:"updatedRange"`
			UpdatedRows  int    `json:"updatedRows"`
		} `json:"updates"`
	}
	if err := r.googleJSON(ctx, credential, http.MethodPost, endpoint,
		map[string]any{"values": rendered.rows}, &out); err != nil {
		return nil, fmt.Errorf("could not append to the spreadsheet: %w", err)
	}
	logger.Info("appended rows to a google spreadsheet", "step", step.Name,
		"document_id", rendered.documentID, "rows", out.Updates.UpdatedRows, "range", out.Updates.UpdatedRange)
	return json.Marshal(googleResult{
		DocumentID:   rendered.documentID,
		URL:          "https://docs.google.com/spreadsheets/d/" + rendered.documentID + "/edit",
		UpdatedRange: out.Updates.UpdatedRange,
		AppendedRows: out.Updates.UpdatedRows,
	})
}

// rowData converts rows into the shape the Sheets creation request takes.
func rowData(rows [][]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		cells := make([]any, 0, len(row))
		for _, cell := range row {
			cells = append(cells, map[string]any{
				"userEnteredValue": map[string]any{"stringValue": fmt.Sprintf("%v", cell)},
			})
		}
		out = append(out, map[string]any{"values": cells})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// googleJSON performs one JSON request against Google as the credential's
// owner. out may be nil when the reply is not needed.
func (r *Runner) googleJSON(ctx context.Context, credential extidentities.Credential,
	method, endpoint string, payload, out any) error {

	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authorize(req, credential)

	resp, err := r.google.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return googleError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// googleStream performs a GET whose body is the caller's to read and close.
// Used for the export, whose response is the file itself.
func (r *Runner) googleStream(ctx context.Context, credential extidentities.Credential, endpoint string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	authorize(req, credential)

	resp, err := r.google.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, googleError(resp)
	}
	return resp.Body, nil
}

// authorize applies the credential. The token is never logged (ADR-0008).
func authorize(req *http.Request, credential extidentities.Credential) {
	tokenType := credential.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	req.Header.Set("Authorization", tokenType+" "+credential.Credentials)
}

// googleError turns a failed response into something a workflow author can
// act on.
//
// Google's error body is quoted rather than replaced: "File not found: 1AbC"
// tells someone their document id is wrong, which no status code does. The
// two statuses worth naming are the ones people actually hit — a document
// they cannot see, and a scope they never granted.
func googleError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		message = parsed.Error.Message
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("google rejected the connected account (HTTP 401: %s); reconnect it and try again", message)
	case http.StatusForbidden:
		return fmt.Errorf("google refused the request (HTTP 403: %s); the connected account may lack the Drive or Docs permission this step needs", message)
	case http.StatusNotFound:
		return fmt.Errorf("google could not find the document (HTTP 404: %s)", message)
	}
	return fmt.Errorf("google returned HTTP %d: %s", resp.StatusCode, message)
}

// withExtension makes sure a stored file's name carries the extension its
// format implies.
//
// It matters more than it looks: an attached file's type is read from its
// NAME, so "Q3 brief" exported as a PDF must be stored as "Q3 brief.pdf" or a
// later agent step cannot attach it at all.
func withExtension(name, extension string) string {
	if strings.EqualFold(strings.TrimSpace(name), "") {
		return "document" + extension
	}
	if strings.HasSuffix(strings.ToLower(name), extension) {
		return name
	}
	return name + extension
}

// googleFields are a step's templated fields, rendered.
//
// Rows are decoded here rather than passed along as text: a template that was
// meant to produce rows and produced something else should fail as "rows must
// be a JSON array of arrays", not as a puzzling response from Google.
type googleFields struct {
	documentID string
	title      string
	body       string
	rangeA1    string
	rows       [][]any
}

// renderGoogleFields renders whichever fields the operation uses.
func renderGoogleFields(step workflows.Step, stepCtx workflows.Context) (googleFields, error) {
	var out googleFields
	render := func(field, template string) (string, error) {
		if template == "" {
			return "", nil
		}
		return workflows.RenderPrompt(step.Name+"."+field, template, stepCtx)
	}
	var err error

	switch step.Operation {
	case workflows.GoogleOperationExport:
		if out.documentID, err = render("document_id", step.DocumentID); err != nil {
			return googleFields{}, err
		}
		out.documentID = strings.TrimSpace(out.documentID)

	case workflows.GoogleOperationCreate:
		if out.title, err = render("title", step.Title); err != nil {
			return googleFields{}, err
		}
		if out.body, err = render("body", step.Body); err != nil {
			return googleFields{}, err
		}
		if out.rows, err = renderRows(step, stepCtx, render); err != nil {
			return googleFields{}, err
		}

	case workflows.GoogleOperationAppend:
		if out.documentID, err = render("document_id", step.DocumentID); err != nil {
			return googleFields{}, err
		}
		out.documentID = strings.TrimSpace(out.documentID)
		if out.rangeA1, err = render("range", step.Range); err != nil {
			return googleFields{}, err
		}
		out.rangeA1 = strings.TrimSpace(out.rangeA1)
		if out.rows, err = renderRows(step, stepCtx, render); err != nil {
			return googleFields{}, err
		}
	}
	return out, nil
}

// renderRows renders the rows template and decodes it.
//
// A single value is accepted as one cell and a flat array as one row, because
// {{ .Previous.rows }} producing ["a","b"] is far more likely to mean one row
// than to be a mistake — and rejecting it would send the author away to write
// a code step that only wraps it in brackets.
func renderRows(step workflows.Step, stepCtx workflows.Context,
	render func(string, string) (string, error)) ([][]any, error) {

	text, err := render("rows", step.Rows)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, fmt.Errorf("rows did not render to JSON (%w); it must produce an array of arrays, for example [[\"a\",1]]", err)
	}
	list, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("rows rendered a %T; it must produce an array of arrays, for example [[\"a\",1]]", decoded)
	}
	rows := make([][]any, 0, len(list))
	flat := false
	for _, entry := range list {
		if row, ok := entry.([]any); ok {
			rows = append(rows, row)
			continue
		}
		flat = true
		break
	}
	if flat {
		// Every entry is a scalar: one row.
		return [][]any{list}, nil
	}
	return rows, nil
}
