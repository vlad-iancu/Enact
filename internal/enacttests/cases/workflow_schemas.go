package cases

import (
	"encoding/json"
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// workflowSchemasCase covers both halves of the schema contract against the
// running services: a trigger payload checked before anything is queued, and
// a code step's declared output checked after it runs.
//
// The enforcement is what makes the schemas worth having. An unenforced
// schema drifts from what the code really returns and then misleads every
// later step — and any editor completing against it.
type workflowSchemasCase struct {
	session    *utils.MainSession
	workflowID string
}

func NewWorkflowSchemas() utils.TestCase { return &workflowSchemasCase{} }

func (c *workflowSchemasCase) Name() string { return "TestWorkflows_Schemas" }

func (c *workflowSchemasCase) Setup(t *utils.T) {
	c.session = t.NewMainSession()
	c.session.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	// The workflow declares what it accepts, and its one code step declares
	// what it returns. The step returns `band` as a NUMBER while its schema
	// says string, so the enforcement has something real to catch.
	body := `{
	  "name": "e2e schema workflow",
	  "input_schema": {
	    "type": "object",
	    "properties": { "customer": {"type": "string"}, "score": {"type": "number"} },
	    "required": ["customer"],
	    "additionalProperties": false
	  },
	  "steps": [
	    {"name":"grade","type":"code",
	     "code":"function run(ctx) { return { band: ctx.input.score > 0.5 ? 'strong' : 1 }; }",
	     "output_schema": {
	       "type": "object",
	       "properties": { "band": {"type": "string"} },
	       "required": ["band"],
	       "additionalProperties": false
	     }}
	  ]
	}`
	var created workflowDTO
	if st := c.session.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID
}

func (c *workflowSchemasCase) Run(t *utils.T) {
	c.checkShapes(t)

	runner := &workflowExecutionCase{session: c.session, workflowID: c.workflowID}

	// A payload that does not match is refused BEFORE anything is queued, so
	// the caller learns at once rather than by polling a doomed execution.
	for _, bad := range []struct {
		what  string
		input string
		says  string
	}{
		{"wrong type", `{"input":{"customer":123}}`, "customer"},
		{"missing required", `{"input":{"score":0.9}}`, "customer"},
		{"unexpected field", `{"input":{"customer":"Ada","surprise":true}}`, "surprise"},
		{"no payload at all", `{}`, "input"},
	} {
		var out executionDTO
		st := c.session.DoJSON(t, http.MethodPost,
			"/workflows/"+c.workflowID+"/executions", strings.NewReader(bad.input), &out)
		if st != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", bad.what, st)
			continue
		}
		if !strings.Contains(strings.ToLower(out.Error), strings.ToLower(bad.says)) {
			t.Errorf("%s: error %q does not point at what is wrong", bad.what, out.Error)
		}
	}

	// A matching payload gets through — otherwise the refusals above would
	// prove only that the endpoint rejects everything.
	execution := runner.runToCompletion(t, `{"input":{"customer":"Ada","score":0.9}}`)
	if execution.Status != "succeeded" {
		t.Fatalf("a valid payload produced %q (%s), want succeeded", execution.Status, execution.Error)
	}

	// score <= 0.5 makes the step return a number for `band`, which its own
	// output_schema forbids — so the step fails on its declared contract.
	failed := runner.runToCompletion(t, `{"input":{"customer":"Ada","score":0.1}}`)
	if failed.Status != "failed" {
		t.Fatalf("a step that broke its output_schema produced %q, want failed", failed.Status)
	}
	if len(failed.Runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(failed.Runs))
	}
	if !strings.Contains(failed.Runs[0].Error, "output_schema") {
		t.Errorf("step error %q does not say the output schema was violated", failed.Runs[0].Error)
	}
	if !strings.Contains(failed.Runs[0].Error, "band") {
		t.Errorf("step error %q does not name the offending field", failed.Runs[0].Error)
	}
}

// checkShapes asserts the resolved-shapes endpoint describes what a step will
// actually receive and produce. It is what an editor builds `ctx` completion
// from, so it has to agree with the run — which the rest of this case then
// exercises for real.
func (c *workflowSchemasCase) checkShapes(t *utils.T) {
	var shapes struct {
		WorkflowID  string          `json:"workflow_id"`
		InputSchema json.RawMessage `json:"input_schema"`
		Steps       []struct {
			Name          string          `json:"name"`
			Type          string          `json:"type"`
			OutputSource  string          `json:"output_source"`
			OutputSchema  json.RawMessage `json:"output_schema"`
			ContextSchema json.RawMessage `json:"context_schema"`
		} `json:"steps"`
		Error string `json:"error"`
	}
	if st := c.session.DoJSON(t, http.MethodGet, "/workflows/"+c.workflowID+"/shapes", nil, &shapes); st != http.StatusOK {
		t.Fatalf("get shapes: got HTTP %d (%s), want 200", st, shapes.Error)
	}
	if shapes.WorkflowID != c.workflowID {
		t.Errorf("shapes are for workflow %q, want %q", shapes.WorkflowID, c.workflowID)
	}
	if len(shapes.InputSchema) == 0 {
		t.Errorf("the workflow's input schema is missing from its shapes")
	}
	if len(shapes.Steps) != 1 {
		t.Fatalf("resolved %d steps, want 1", len(shapes.Steps))
	}
	step := shapes.Steps[0]
	// A code step's shape is its own declaration, not derived from anywhere.
	if step.OutputSource != "step" {
		t.Errorf("output_source = %q, want \"step\"", step.OutputSource)
	}
	if !strings.Contains(string(step.OutputSchema), "band") {
		t.Errorf("resolved output schema %s does not carry the declared shape", step.OutputSchema)
	}
	// The context schema must describe the trigger input this step can reach,
	// and — being the first step — no previous one.
	var ctxSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(step.ContextSchema, &ctxSchema); err != nil {
		t.Fatalf("context_schema is not valid JSON: %v (%s)", err, step.ContextSchema)
	}
	if _, has := ctxSchema.Properties["input"]; !has {
		t.Errorf("context schema does not declare `input`")
	}
	if _, has := ctxSchema.Properties["previous"]; has {
		t.Errorf("the first step's context declares `previous`; there is no previous step")
	}
	if !strings.Contains(string(ctxSchema.Properties["input"]), "customer") {
		t.Errorf("context `input` is not the workflow's input schema: %s", ctxSchema.Properties["input"])
	}
}

func (c *workflowSchemasCase) TearDown(t *utils.T) {
	if c.workflowID == "" || c.session == nil {
		return
	}
	if st := c.session.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
		t.Errorf("teardown: delete workflow got HTTP %d", st)
	}
}
