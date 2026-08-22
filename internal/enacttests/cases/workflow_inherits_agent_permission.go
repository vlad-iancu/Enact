package cases

import (
	"net/http"
	"strings"

	"enact/internal/enacttests/utils"
)

// workflowInheritsAgentPermissionCase is the security property the whole
// design leans on.
//
// A workflow's agent step runs through enact-model-inference AS THE USER WHO
// TRIGGERED IT, and that service already refuses an agent the user may not
// use. So being allowed to run a workflow must NOT confer the right to run
// the agents inside it — otherwise anyone given a workflow could reach every
// agent its author could, and workflows would become a permission laundry.
//
// Nothing in the workflow services re-implements that check, which is exactly
// why it needs a test: an inherited guarantee is one nobody notices losing.
type workflowInheritsAgentPermissionCase struct {
	owner      *utils.MainSession
	other      *utils.MainSession
	agentID    string
	workflowID string
}

func NewWorkflowInheritsAgentPermission() utils.TestCase {
	return &workflowInheritsAgentPermissionCase{}
}

func (c *workflowInheritsAgentPermissionCase) Name() string {
	return "TestWorkflows_InheritsAgentPermission"
}

func (c *workflowInheritsAgentPermissionCase) Setup(t *utils.T) {
	c.owner = t.NewMainSession()
	c.owner.RegisterOrLogin(t, "E2E Main", mainTestEmail, mainTestPassword)

	// An agent belonging to the owner. The other user is never given any rule
	// for it.
	var agent workflowDTO
	if st := c.owner.DoJSON(t, http.MethodPost, "/agents", strings.NewReader(
		`{"name":"e2e private agent","model":"claude-sonnet-4-6","knowledge_base_ids":[],"system_prompt":"Say OK."}`),
		&agent); st != http.StatusCreated {
		t.Fatalf("create agent: got HTTP %d (%s), want 201", st, agent.Error)
	}
	c.agentID = agent.ID

	var created workflowDTO
	body := `{"name":"e2e borrowed agent","steps":[
		{"name":"call","type":"agent","agent_id":"` + c.agentID + `","prompt":"Say OK."}]}`
	if st := c.owner.DoJSON(t, http.MethodPost, "/workflows", strings.NewReader(body), &created); st != http.StatusCreated {
		t.Fatalf("create workflow: got HTTP %d (%s), want 201", st, created.Error)
	}
	c.workflowID = created.ID

	c.other = t.NewMainSession()
	c.other.RegisterOrLogin(t, "E2E Other", mainOtherEmail, mainTestPassword)

	// The other user may SEE and RUN the workflow — but is deliberately given
	// nothing at all for the agent inside it.
	if err := t.Env.GrantRules(t.Context(), c.other.UserID(), []string{
		"enact:workflow:view:" + c.workflowID,
		"enact:workflow:use:" + c.workflowID,
	}); err != nil {
		t.Fatalf("grant workflow rules: %v", err)
	}
}

func (c *workflowInheritsAgentPermissionCase) Run(t *utils.T) {
	// They can indeed reach the workflow: the grant worked, so a refusal
	// below is about the agent and not about the workflow.
	var visible workflowDTO
	if st := c.other.DoJSON(t, http.MethodGet, "/workflows/"+c.workflowID, nil, &visible); st != http.StatusOK {
		t.Fatalf("granted user get workflow: got HTTP %d, want 200 — the grant did not take, so this case proves nothing", st)
	}
	if !visible.Usable {
		t.Fatalf("granted user does not see the workflow as usable; the rest of this case would be vacuous")
	}

	inner := &workflowExecutionCase{session: c.other, workflowID: c.workflowID}
	execution := inner.runToCompletion(t, `{}`)

	// The run is accepted — they may run the workflow — and then fails on the
	// step, because the agent is not theirs to use.
	if execution.Status != "failed" {
		t.Fatalf("execution status = %q, want failed: running a workflow must not confer the right to run its agents", execution.Status)
	}
	if len(execution.Runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(execution.Runs))
	}
	run := execution.Runs[0]
	if run.Status != "failed" {
		t.Errorf("step status = %q, want failed", run.Status)
	}
	// The refusal comes from the inference service, relayed verbatim, rather
	// than from anything the workflow services decided for themselves.
	if !strings.Contains(strings.ToLower(run.Error), "permission") &&
		!strings.Contains(strings.ToLower(run.Error), "not found") {
		t.Errorf("step error %q does not read like an authorization refusal", run.Error)
	}
}

func (c *workflowInheritsAgentPermissionCase) TearDown(t *utils.T) {
	if c.owner == nil {
		return
	}
	if c.workflowID != "" {
		if st := c.owner.DoJSON(t, http.MethodDelete, "/workflows/"+c.workflowID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
			t.Errorf("teardown: delete workflow got HTTP %d", st)
		}
	}
	if c.agentID != "" {
		if st := c.owner.DoJSON(t, http.MethodDelete, "/agents/"+c.agentID, nil, nil); st != http.StatusNoContent && st != http.StatusNotFound {
			t.Errorf("teardown: delete agent got HTTP %d", st)
		}
	}
}
