package cases

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"enact/internal/enacttests/utils"
)

// workflowExecutionCase runs a real workflow end to end through the queue and
// the runner, and asserts what each step saw.
//
// Every step is a CODE step, deliberately. That exercises the whole
// machinery — intake, the Redis stream, the runner, the step context, the
// per-step record — without spending a model call, so the suite stays fast,
// free and deterministic. Agent steps are covered by their own validation
// case and verified against Bedrock by hand.
type workflowExecutionCase struct {
	session    *utils.MainSession
	workflowID string
}

func NewWorkflowExecution() utils.TestCase { return &workflowExecutionCase{} }

func (c *workflowExecutionCase) Name() string { return "TestWorkflows_ExecutionContext" }

type workflowDTO struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Steps  json.RawMessage `json:"steps"`
	Usable bool            `json:"usable"`
	Error  string          `json:"error"`
}

type stepRunDTO struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

type executionDTO struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
	Runs   []stepRunDTO    `json:"runs"`
	APIErr string          `json:"error_message"`
}

func (c *workflowExecutionCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	// Step two reaches the TRIGGER INPUT and step one's output by name — the
	// two things a strict pipeline could not do.
	body := `{
	  "name": "e2e code workflow",
	  "steps": [
	    {"name":"double","type":"code",
	     "code":"function run(input) { return { doubled: input.input.n * 2 }; }"},
	    {"name":"combine","type":"code",
	     "code":"function run(input) { return { total: input.steps.double.doubled + input.input.n, from_previous: input.previous.doubled, label: input.input.label }; }"}
	  ]
	}`
	var created workflowDTO
	if st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID
}

func (c *workflowExecutionCase) Run(t *utils.T) {
	execution := c.runToCompletion(t, `{"input":{"n":21,"label":"answer"}}`)

	if execution.Status != "succeeded" {
		t.Fatalf("execution status = %q (%s), want succeeded", execution.Status, execution.Error)
	}
	if len(execution.Runs) != 2 {
		t.Fatalf("recorded %d step runs, want 2", len(execution.Runs))
	}
	for _, run := range execution.Runs {
		if run.Status != "succeeded" {
			t.Errorf("step %q status = %q (%s)", run.Name, run.Status, run.Error)
		}
		// The input is recorded too, which is what makes a failure
		// diagnosable after the fact.
		if len(run.Input) == 0 {
			t.Errorf("step %q recorded no input", run.Name)
		}
	}

	var final struct {
		Total        int    `json:"total"`
		FromPrevious int    `json:"from_previous"`
		Label        string `json:"label"`
	}
	if err := json.Unmarshal(execution.Output, &final); err != nil {
		t.Fatalf("decode final output: %v (%s)", err, execution.Output)
	}
	// 42 from step one, plus 21 from the trigger input: proof that a step can
	// reach past its predecessor.
	if final.Total != 63 {
		t.Errorf("total = %d, want 63 (steps.double.doubled + input.n)", final.Total)
	}
	if final.FromPrevious != 42 {
		t.Errorf("from_previous = %d, want 42", final.FromPrevious)
	}
	if final.Label != "answer" {
		t.Errorf("label = %q, want the trigger input to be reachable from step 2", final.Label)
	}
}

// runToCompletion triggers the workflow and polls until it settles.
func (c *workflowExecutionCase) runToCompletion(t *utils.T, input string) executionDTO {
	var queued executionDTO
	if st := c.session.DoJSON(t, http.MethodPost,
		"/workflows/"+c.workflowID+"/executions", strings.NewReader(input), &queued); st != http.StatusAccepted {
		t.Fatalf("trigger workflow: got HTTP %d (%s), want 202", st, queued.Error)
	}
	if queued.Status != "queued" {
		t.Errorf("a freshly triggered execution has status %q, want queued", queued.Status)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var current executionDTO
		if st := c.session.DoJSON(t, http.MethodGet, "/workflows/executions/"+queued.ID, nil, &current); st != http.StatusOK {
			t.Fatalf("poll execution: got HTTP %d", st)
		}
		if current.Status == "succeeded" || current.Status == "failed" {
			return current
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("execution %s did not finish within the deadline", queued.ID)
	return executionDTO{}
}

func (c *workflowExecutionCase) TearDown(t *utils.T) {
	if c.workflowID == "" || c.session == nil {
		return
	}
	if st := c.session.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: delete workflow got HTTP %d", st)
	}
}

// ---------------------------------------------------------------------------

// workflowFailureCase verifies that a failing step stops the run, is recorded
// as the cause, and leaves the steps after it visibly unrun rather than
// silently absent.
type workflowFailureCase struct {
	session    *utils.MainSession
	workflowID string
}

func NewWorkflowFailure() utils.TestCase { return &workflowFailureCase{} }

func (c *workflowFailureCase) Name() string { return "TestWorkflows_StepFailureStopsTheRun" }

func (c *workflowFailureCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	body := `{
	  "name": "e2e failing workflow",
	  "steps": [
	    {"name":"ok","type":"code","code":"function run(input) { return { fine: true }; }"},
	    {"name":"boom","type":"code","code":"function run(input) { throw new Error('deliberate failure'); }"},
	    {"name":"never","type":"code","code":"function run(input) { return { reached: true }; }"}
	  ]
	}`
	var created workflowDTO
	if st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID
}

func (c *workflowFailureCase) Run(t *utils.T) {
	inner := &workflowExecutionCase{session: c.session, workflowID: c.workflowID}
	execution := inner.runToCompletion(t, `{}`)

	if execution.Status != "failed" {
		t.Fatalf("execution status = %q, want failed", execution.Status)
	}
	if !strings.Contains(execution.Error, "boom") {
		t.Errorf("execution error %q does not name the step that failed", execution.Error)
	}
	if len(execution.Runs) != 3 {
		t.Fatalf("recorded %d runs, want 3 — the steps after a failure must be recorded, not omitted", len(execution.Runs))
	}
	want := map[string]string{"ok": "succeeded", "boom": "failed", "never": "skipped"}
	for _, run := range execution.Runs {
		if got := want[run.Name]; run.Status != got {
			t.Errorf("step %q status = %q, want %q", run.Name, run.Status, got)
		}
	}
	for _, run := range execution.Runs {
		if run.Name == "boom" && !strings.Contains(run.Error, "deliberate failure") {
			t.Errorf("the thrown message was lost: %q", run.Error)
		}
	}
	if len(execution.Output) != 0 && string(execution.Output) != "null" {
		t.Errorf("a failed execution carries output %s; it should carry none", execution.Output)
	}
}

func (c *workflowFailureCase) TearDown(t *utils.T) {
	if c.workflowID == "" || c.session == nil {
		return
	}
	if st := c.session.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: delete workflow got HTTP %d", st)
	}
}

// ---------------------------------------------------------------------------

// workflowValidationCase verifies that a broken workflow is refused when it is
// SAVED. That matters more here than elsewhere: a run may not reach step seven
// for several minutes and several model calls, so a mistake discovered then is
// discovered expensively.
type workflowValidationCase struct {
	utils.BaseCase
	session *utils.MainSession
}

func NewWorkflowValidation() utils.TestCase { return &workflowValidationCase{} }

func (c *workflowValidationCase) Name() string { return "TestWorkflows_RejectsBrokenDefinitions" }

func (c *workflowValidationCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)
}

func (c *workflowValidationCase) Run(t *utils.T) {
	cases := []struct {
		what string
		body string
		says string
	}{
		{"no steps", `{"name":"x","steps":[]}`, "at least one step"},
		{"duplicate names", `{"name":"x","steps":[
			{"name":"a","type":"code","code":"function run(i){return 1;}"},
			{"name":"a","type":"code","code":"function run(i){return 2;}"}]}`, "unique"},
		{"unaddressable name", `{"name":"x","steps":[
			{"name":"my step","type":"code","code":"function run(i){return 1;}"}]}`, "step name"},
		{"unknown type", `{"name":"x","steps":[{"name":"a","type":"magic"}]}`, "expected"},
		{"broken template", `{"name":"x","steps":[
			{"name":"a","type":"agent","agent_id":"nope","prompt":"{{ .Input "}]}`, "template"},
		{"missing agent", `{"name":"x","steps":[
			{"name":"a","type":"agent","agent_id":"00000000-0000-0000-0000-000000000000","prompt":"hi"}]}`, "not found"},
		{"no name", `{"name":"","steps":[{"name":"a","type":"code","code":"function run(i){return 1;}"}]}`, "name is required"},
	}
	for _, tc := range cases {
		var out workflowDTO
		st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(tc.body), &out)
		if st != http.StatusBadRequest {
			// Should never happen, but if it was accepted, clean it up.
			if out.ID != "" {
				c.session.DoJSON(t, http.MethodDelete, "/workflows/"+out.ID, nil, nil)
			}
			t.Errorf("%s: got HTTP %d, want 400", tc.what, st)
			continue
		}
		if !strings.Contains(strings.ToLower(out.Error), strings.ToLower(tc.says)) {
			t.Errorf("%s: error %q does not mention %q", tc.what, out.Error, tc.says)
		}
	}

	// A step count beyond the cap is refused: it is the only ceiling on what
	// one execution can cost.
	steps := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		steps = append(steps, fmt.Sprintf(`{"name":"s%d","type":"code","code":"function run(i){return 1;}"}`, i))
	}
	var out workflowDTO
	body := `{"name":"too many","steps":[` + strings.Join(steps, ",") + `]}`
	if st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &out); st != http.StatusBadRequest {
		if out.ID != "" {
			c.session.DoJSON(t, http.MethodDelete, "/workflows/"+out.ID, nil, nil)
		}
		t.Errorf("30 steps: got HTTP %d, want 400", st)
	}
}
