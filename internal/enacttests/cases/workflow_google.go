package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// workflowGoogleDocsCase covers a provider-backed step without calling Google.
//
// The two things worth proving here are the two that do not need a Google
// account: a provider that does not exist is refused when the workflow is
// SAVED, and a step whose provider the running user has not connected fails
// with something they can act on. The Google API calls themselves are verified
// by hand against a real account — a suite that created documents in somebody's
// Drive on every run would be a poor trade.
type workflowGoogleDocsCase struct {
	session    *utils.MainSession
	workflowID string
}

func NewWorkflowGoogleDocs() utils.TestCase { return &workflowGoogleDocsCase{} }

func (c *workflowGoogleDocsCase) Name() string { return "TestWorkflows_GoogleSteps" }

func (c *workflowGoogleDocsCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)
}

func (c *workflowGoogleDocsCase) Run(t *utils.T) {
	// Shape errors are caught before anything reaches a service.
	for _, bad := range []struct {
		what string
		step string
		says string
	}{
		{"no provider", `{"name":"a","type":"google-docs","operation":"export","document_id":"x"}`, "provider"},
		{"no operation", `{"name":"a","type":"google-docs","provider":"gmail","document_id":"x"}`, "operation"},
		{"export without document_id", `{"name":"a","type":"google-docs","provider":"gmail","operation":"export"}`, "document_id"},
		{"unknown format", `{"name":"a","type":"google-docs","provider":"gmail","operation":"export","document_id":"x","format":"rtf"}`, "offers"},
		{"create without title", `{"name":"a","type":"google-docs","provider":"gmail","operation":"create"}`, "title"},
		{"unknown provider", `{"name":"a","type":"google-docs","provider":"definitely-not-a-provider","operation":"export","document_id":"x"}`, "not found"},

		// Operations and formats are per type, not shared: a document cannot
		// append, and a spreadsheet cannot export as docx.
		{"append on a document", `{"name":"a","type":"google-docs","provider":"gmail","operation":"append","document_id":"x","rows":"[[1]]"}`, "supports"},
		{"append on a deck", `{"name":"a","type":"google-slides","provider":"gmail","operation":"append","document_id":"x","rows":"[[1]]"}`, "supports"},
		{"docx from a spreadsheet", `{"name":"a","type":"google-sheets","provider":"gmail","operation":"export","document_id":"x","format":"docx"}`, "offers"},
		{"csv from a deck", `{"name":"a","type":"google-slides","provider":"gmail","operation":"export","document_id":"x","format":"csv"}`, "offers"},
		{"body on a deck", `{"name":"a","type":"google-slides","provider":"gmail","operation":"create","title":"t","body":"x"}`, "created empty"},
		{"rows on a document", `{"name":"a","type":"google-docs","provider":"gmail","operation":"create","title":"t","rows":"[[1]]"}`, "only a spreadsheet"},
		{"append without rows", `{"name":"a","type":"google-sheets","provider":"gmail","operation":"append","document_id":"x"}`, "rows"},
	} {
		var out workflowDTO
		st := c.session.DoJSON(t, http.MethodPost, "/workflows",
			strings.NewReader(`{"name":"x","steps":[`+bad.step+`]}`), &out)
		if st != http.StatusBadRequest {
			if out.ID != "" {
				c.session.DoJSON(t, http.MethodDelete, "/workflows/"+out.ID, nil, nil)
			}
			t.Errorf("%s: got HTTP %d, want 400", bad.what, st)
			continue
		}
		if !strings.Contains(strings.ToLower(out.Error), strings.ToLower(bad.says)) {
			t.Errorf("%s: error %q does not mention %q", bad.what, out.Error, bad.says)
		}
	}

	// Each type's well-formed step saves and advertises a fixed output shape.
	for _, good := range []struct {
		what string
		step string
	}{
		{"sheets export", `{"name":"s","type":"google-sheets","provider":"gmail","operation":"export","document_id":"{{ .Input.id }}","format":"csv"}`},
		{"sheets append", `{"name":"s","type":"google-sheets","provider":"gmail","operation":"append","document_id":"{{ .Input.id }}","rows":"{{ .Input.rows }}"}`},
		{"slides export", `{"name":"s","type":"google-slides","provider":"gmail","operation":"export","document_id":"{{ .Input.id }}","format":"pdf"}`},
		{"slides create", `{"name":"s","type":"google-slides","provider":"gmail","operation":"create","title":"Deck"}`},
	} {
		var out workflowDTO
		st := c.session.DoJSON(t, http.MethodPost, "/workflows",
			strings.NewReader(`{"name":"x","steps":[`+good.step+`]}`), &out)
		if st != http.StatusCreated {
			t.Errorf("%s: got HTTP %d (%s), want 201", good.what, st, out.Error)
			continue
		}
		var shapes struct {
			Steps []struct {
				OutputSource string `json:"output_source"`
			} `json:"steps"`
		}
		if st := c.session.DoJSON(t, http.MethodGet, "/workflows/"+out.ID+"/shapes", nil, &shapes); st != http.StatusOK {
			t.Errorf("%s: shapes got HTTP %d", good.what, st)
		} else if len(shapes.Steps) != 1 || shapes.Steps[0].OutputSource != "operation" {
			t.Errorf("%s: output_source = %v, want \"operation\"", good.what, shapes.Steps)
		}
		c.session.DoJSON(t, http.MethodDelete, "/workflows/"+out.ID, nil, nil)
	}

	// A well-formed step naming a real provider saves.
	var created workflowDTO
	body := `{"name":"e2e google docs","steps":[
		{"name":"brief","type":"google-docs","provider":"gmail","operation":"export",
		 "document_id":"{{ .Input.doc_id }}","format":"pdf"}]}`
	if st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID

	// Its output shape is fixed by the operation, not declared — which is what
	// lets a later step attach steps.brief.file without anyone writing a schema.
	var shapes struct {
		Steps []struct {
			OutputSource string `json:"output_source"`
			OutputSchema string `json:"-"`
		} `json:"steps"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/workflows/"+c.workflowID+"/shapes", nil, &shapes); st != http.StatusOK {
		t.Fatalf("get shapes: got HTTP %d, want 200", st)
	}
	if len(shapes.Steps) != 1 || shapes.Steps[0].OutputSource != "operation" {
		t.Errorf("output_source = %v, want \"operation\"", shapes.Steps)
	}

	// Running it as somebody who has not connected that provider fails on the
	// step, with the account — not the workflow — named as what is missing.
	runner := &workflowExecutionCase{session: c.session, workflowID: c.workflowID}
	execution := runner.runToCompletion(t, `{"input":{"doc_id":"whatever"}}`)
	if execution.Status != "failed" {
		t.Fatalf("execution status = %q, want failed: nobody has connected this provider", execution.Status)
	}
	if len(execution.Runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(execution.Runs))
	}
	if !strings.Contains(execution.Runs[0].Error, "connected") {
		t.Errorf("step error %q does not tell the user to connect an account", execution.Runs[0].Error)
	}
}

func (c *workflowGoogleDocsCase) TearDown(t *utils.T) {
	if c.workflowID == "" || c.session == nil {
		return
	}
	if st := c.session.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: delete workflow got HTTP %d", st)
	}
}
