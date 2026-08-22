package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// workflowIsolationCase verifies that a workflow is another user's business
// only if they were given it, and that RUNNING one is a permission distinct
// from reading it.
//
// The second half matters most. A workflow is a way to spend model calls
// against someone else's agents; if "can see it" implied "can run it", every
// viewer would have that power.
type workflowIsolationCase struct {
	owner      *utils.MainSession
	other      *utils.MainSession
	workflowID string
}

func NewWorkflowIsolation() utils.TestCase { return &workflowIsolationCase{} }

func (c *workflowIsolationCase) Name() string { return "TestWorkflows_Isolation" }

func (c *workflowIsolationCase) Setup(t *utils.T) {
	c.owner = t.NewMainSession()
	c.owner.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	body := `{"name":"e2e private workflow","steps":[
		{"name":"only","type":"code","code":"function run(input) { return { ok: true }; }"}]}`
	var created workflowDTO
	if st := c.owner.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID

	c.other = t.NewMainSession()
	c.other.RegisterOrLogin(t, "E2E Other", mainOtherEmail, mainTestPassword)
}

func (c *workflowIsolationCase) Run(t *utils.T) {
	// The owner sees it, and may run it.
	var mine workflowDTO
	if st := c.owner.DoJSON(t, http.MethodGet, "/workflows/"+c.workflowID, nil, &mine); st != http.StatusOK {
		t.Fatalf("owner get: got HTTP %d, want 200", st)
	}
	if !mine.Usable {
		t.Errorf("the owner's own workflow is not marked usable")
	}

	// Another member of the same organization, with no rule for it, cannot
	// see it — and gets 404 rather than 403, so "not yours" is
	// indistinguishable from "does not exist".
	for _, probe := range []struct {
		what   string
		method string
		path   string
		body   string
		want   int
	}{
		{"get", http.MethodGet, "/workflows/" + c.workflowID, "", http.StatusNotFound},
		{"update", http.MethodPut, "/workflows/" + c.workflowID, `{"name":"hijack"}`, http.StatusNotFound},
		{"delete", http.MethodDelete, "/workflows/" + c.workflowID, "", http.StatusNotFound},
		{"run", http.MethodPost, "/workflows/" + c.workflowID + "/executions", `{}`, http.StatusNotFound},
		{"executions", http.MethodGet, "/workflows/" + c.workflowID + "/executions", "", http.StatusNotFound},
	} {
		var reader *strings.Reader
		if probe.body != "" {
			reader = strings.NewReader(probe.body)
		}
		var st int
		if reader != nil {
			st = c.other.DoJSON(t, probe.method, probe.path, reader, nil)
		} else {
			st = c.other.DoJSON(t, probe.method, probe.path, nil, nil)
		}
		if st != probe.want {
			t.Errorf("other user %s: got HTTP %d, want %d", probe.what, st, probe.want)
		}
	}

	// ...and it is absent from their listing, not merely unreadable by id.
	var listing struct {
		Workflows []workflowDTO `json:"workflows"`
	}
	if st := c.other.DoJSON(t, http.MethodGet, "/workflows", nil, &listing); st != http.StatusOK {
		t.Fatalf("other user list: got HTTP %d, want 200", st)
	}
	for _, w := range listing.Workflows {
		if w.ID == c.workflowID {
			t.Errorf("another user's listing includes a workflow they cannot read")
		}
	}

	// An execution is reachable only through its workflow's permissions.
	var queued executionDTO
	if st := c.owner.DoJSON(t, http.MethodPost,
		"/workflows/"+c.workflowID+"/executions", strings.NewReader(`{}`), &queued); st != http.StatusAccepted {
		t.Fatalf("owner run: got HTTP %d, want 202", st)
	}
	if st := c.other.DoJSON(t, http.MethodGet, "/workflows/executions/"+queued.ID, nil, nil); st != http.StatusNotFound {
		t.Errorf("other user reading the owner's execution: got HTTP %d, want 404", st)
	}
}

func (c *workflowIsolationCase) TearDown(t *utils.T) {
	if c.workflowID == "" || c.owner == nil {
		return
	}
	if st := c.owner.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: delete workflow got HTTP %d", st)
	}
}
