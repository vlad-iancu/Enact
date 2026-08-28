package enactworkflows

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/agents"
	"enact/internal/extidentities"
	"enact/internal/files"
	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/workflows"
)

// maxInputBytes bounds a trigger payload. It is stored on the execution record
// and interpolated into prompts, so an unbounded one is paid for repeatedly.
const maxInputBytes = 256 << 10 // 256 KiB

// WorkflowAPI serves workflow CRUD and execution intake.
type WorkflowAPI struct {
	repo       *workflows.Repository
	executions *workflows.ExecutionRepository
	// agents validates that a step's agent exists and is visible to the
	// caller, asking the service that owns that domain rather than reading its
	// storage.
	agents *agents.Client
	// identities validates the providers a step draws credentials from, asking
	// the service that owns that domain.
	identities *extidentities.Client
	producer   *queue.Producer
	rbac       *rbac.Client
	enforcer   *rbac.Enforcer
	// files holds what a workflow's runs produced. This service never reads
	// them — the runner writes and enact-main serves — but it owns deleting a
	// workflow, and a delete that left its files behind would leak storage
	// nothing can reach.
	files  files.Store
	logger *logging.Logger
}

func newWorkflowAPI(repo *workflows.Repository, executions *workflows.ExecutionRepository,
	agentClient *agents.Client, identitiesClient *extidentities.Client, producer *queue.Producer,
	rbacClient *rbac.Client, enforcer *rbac.Enforcer, fileStore files.Store,
	logger *logging.Logger) *WorkflowAPI {
	return &WorkflowAPI{
		repo: repo, executions: executions, agents: agentClient, identities: identitiesClient, producer: producer,
		rbac: rbacClient, enforcer: enforcer, files: fileStore, logger: logger,
	}
}

type saveRequest struct {
	Name string `json:"name"`
	// Description is a pointer so an update can tell "not mentioned" from
	// "set to nothing". As a plain string the two are the same value, which
	// made a description impossible to remove: every empty one read as
	// "leave it alone", so the old text stayed and the caller was told the
	// save succeeded.
	Description *string          `json:"description"`
	InputSchema json.RawMessage  `json:"input_schema"`
	Steps       []workflows.Step `json:"steps"`
}

// description reads the field for a create, where absent and empty mean the
// same thing.
func (r saveRequest) description() string {
	if r.Description == nil {
		return ""
	}
	return strings.TrimSpace(*r.Description)
}

type triggerRequest struct {
	Input json.RawMessage `json:"input"`
}

type listWorkflowsResponse struct {
	Workflows []workflows.Workflow `json:"workflows"`
}

type listExecutionsResponse struct {
	Executions []workflows.Execution `json:"executions"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (a *WorkflowAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/workflows").
		To(a.create).
		Reads(saveRequest{}).
		Doc("Create a workflow").
		Returns(http.StatusCreated, "Created", workflows.Workflow{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("/workflows").
		To(a.list).
		Doc("List the workflows the caller may see").
		Returns(http.StatusOK, "OK", listWorkflowsResponse{}))

	ws.Route(ws.GET("/workflows/{id}").
		To(a.get).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Get a workflow").
		Returns(http.StatusOK, "OK", workflows.Workflow{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/workflows/{id}").
		To(a.update).
		Param(ws.PathParameter("id", "workflow id")).
		Reads(saveRequest{}).
		Doc("Update a workflow: provided fields change, absent ones keep their value").
		Returns(http.StatusOK, "OK", workflows.Workflow{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/workflows/{id}").
		To(a.delete).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Delete a workflow and its execution history").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/workflows/{id}/executions").
		To(a.trigger).
		Param(ws.PathParameter("id", "workflow id")).
		Reads(triggerRequest{}).
		Doc("Queue an execution. Returns immediately with a record in state \"queued\"; poll GET /v1/executions/{id} for progress").
		Returns(http.StatusAccepted, "Queued", workflows.Execution{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/workflows/{id}/shapes").
		To(a.shapes).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Resolved input and output shapes per step: an agent step's from its agent, a code step's from its own declaration, plus the context schema each step receives (for editor completion on ctx)").
		Returns(http.StatusOK, "OK", workflows.Shapes{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/workflows/{id}/executions").
		To(a.listExecutions).
		Param(ws.PathParameter("id", "workflow id")).
		Param(ws.QueryParameter("limit", "how many to return (default 50, max 200)").DataType("integer")).
		Doc("List a workflow's executions, newest first").
		Returns(http.StatusOK, "OK", listExecutionsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/executions/{id}").
		To(a.getExecution).
		Param(ws.PathParameter("id", "execution id")).
		Doc("Get one execution, with what every step received and produced").
		Returns(http.StatusOK, "OK", workflows.Execution{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

// organization resolves the caller's organization for a scoped lookup,
// writing the refusal itself when they have none. Every read passes through
// it, so no path can accidentally fetch across the boundary.
func (a *WorkflowAPI) organization(req *restful.Request, resp *restful.Response, notFound string) (string, bool) {
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDenied(req, resp, err, notFound)
		return "", false
	}
	return organizationID, true
}

// validate checks everything about a workflow that can be known at save time.
//
// Step shape is the domain's business (workflows.ValidateSteps); what needs a
// service call — do these agents exist, and may this caller see them — is
// this handler's, because only it has a client.
func (a *WorkflowAPI) validate(req *restful.Request, logger *logging.Logger, body saveRequest) (string, bool) {
	if strings.TrimSpace(body.Name) == "" {
		return "name is required", false
	}
	// Compiled here because it is enforced on every trigger: a schema that
	// cannot compile would refuse every run.
	if _, err := workflows.CompileSchema(body.InputSchema); err != nil {
		logger.Warn("workflow input schema rejected", "err", err)
		return fmt.Sprintf("invalid input_schema: %s", err), false
	}
	if msg, ok := workflows.ValidateSteps(body.Steps); !ok {
		logger.Warn("workflow validation failed", "reason", msg)
		return msg, false
	}
	// A provider that does not exist is a step that can only ever fail, and
	// the failure would land mid-run — so it is refused here, where the author
	// can still fix it. Whether the RUNNING user has connected an account at
	// that provider is deliberately not checked: that is per-person and
	// per-run, and a workflow is authored once for everybody.
	if providers := workflows.Providers(body.Steps); len(providers) > 0 {
		known, err := a.identities.Providers(req.Request.Context())
		if err != nil {
			logger.Error("failed to validate providers", "err", err)
			return "failed to validate the providers this workflow references", false
		}
		names := make(map[string]bool, len(known))
		for _, p := range known {
			names[p.Name] = true
		}
		for _, provider := range providers {
			if !names[provider] {
				logger.Warn("workflow references a missing provider", "provider", provider)
				return fmt.Sprintf("provider %q not found; connect it under Accounts first", provider), false
			}
		}
	}
	for _, agentID := range workflows.AgentIDs(body.Steps) {
		_, found, err := a.agents.Get(req.Request.Context(), agentID)
		if err != nil {
			logger.Error("failed to validate agent", "agent_id", agentID, "err", err)
			return "failed to validate the agents this workflow references", false
		}
		if !found {
			logger.Warn("workflow references a missing agent", "agent_id", agentID)
			return fmt.Sprintf("agent %q not found", agentID), false
		}
	}
	return "", true
}

func (a *WorkflowAPI) create(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("create workflow requested")
	if err := a.enforcer.Require(req.Request.Context(),
		rbac.Permission(rbac.ResourceWorkflow, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("create workflow denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	body, ok := decodeSave(req, resp, logger)
	if !ok {
		return
	}
	if msg, valid := a.validate(req, logger, body); !valid {
		writeError(req, resp, http.StatusBadRequest, msg)
		return
	}
	organizationID, ok := a.organization(req, resp, "workflow not found")
	if !ok {
		return
	}
	now := time.Now().UTC()
	workflow := workflows.Workflow{
		ID:             uuid.NewString(),
		UserID:         userID,
		OrganizationID: organizationID,
		Name:           strings.TrimSpace(body.Name),
		Description:    body.description(),
		InputSchema:    body.InputSchema,
		Steps:          withStepIDs(body.Steps),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.repo.Create(req.Request.Context(), workflow); err != nil {
		logger.Error("failed to create workflow", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to create the workflow")
		return
	}
	// The creator owns it. Recorded before replying, so a client that reads
	// the workflow back immediately can.
	if err := a.rbac.Grant(req.Request.Context(), rbac.GrantRequest{
		UserID:     userID,
		Resource:   rbac.ResourceWorkflow,
		ResourceID: workflow.ID,
	}); err != nil {
		logger.Error("failed to record ownership", "workflow_id", workflow.ID, "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to record ownership of the workflow")
		return
	}
	// The caller's cached rules predate this grant; without dropping them they
	// would be refused the workflow they just created until the TTL expired.
	a.enforcer.Forget(userID)
	logger.Info("workflow created", "workflow_id", workflow.ID, "name", workflow.Name, "steps", len(workflow.Steps))
	requesthelper.WriteJSON(req, resp, http.StatusCreated, workflow)
}

func (a *WorkflowAPI) list(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("list workflows requested")
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	effective, err := a.enforcer.CallerEffective(req.Request.Context())
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	records, err := a.repo.List(req.Request.Context(), organizationID)
	if err != nil {
		logger.Error("failed to list workflows", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	// Candidates are the organization's; the caller's rules decide which they
	// may see. A user with no roles still gets their own, through the hidden
	// ownership role rather than a hard-coded owner filter.
	out := make([]workflows.Workflow, 0, len(records))
	for _, w := range records {
		if !effective.Allows(rbac.Permission(rbac.ResourceWorkflow, rbac.ActionView, w.ID)) {
			continue
		}
		out = append(out, w)
	}
	logger.Info("workflows listed", "candidates", len(records), "visible", len(out), "organization_id", organizationID)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listWorkflowsResponse{Workflows: out})
}

// load fetches a workflow after checking one permission on it. Every
// single-workflow route begins here, so the permission check and the
// organization scoping cannot drift apart.
func (a *WorkflowAPI) load(req *restful.Request, resp *restful.Response, logger *logging.Logger, action string) (workflows.Workflow, bool) {
	id := req.PathParameter("id")
	if err := a.enforcer.RequireResource(req.Request.Context(), rbac.ResourceWorkflow, action, id); err != nil {
		logger.Warn("workflow access denied", "action", action, "err", err)
		rbac.WriteDenied(req, resp, err, "workflow not found")
		return workflows.Workflow{}, false
	}
	organizationID, ok := a.organization(req, resp, "workflow not found")
	if !ok {
		return workflows.Workflow{}, false
	}
	workflow, found, err := a.repo.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to load workflow", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to load the workflow")
		return workflows.Workflow{}, false
	}
	if !found {
		logger.Warn("workflow not found")
		writeError(req, resp, http.StatusNotFound, "workflow not found")
		return workflows.Workflow{}, false
	}
	return workflow, true
}

func (a *WorkflowAPI) get(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"))
	logger.Info("get workflow requested")
	workflow, ok := a.load(req, resp, logger, rbac.ActionView)
	if !ok {
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, workflow)
}

func (a *WorkflowAPI) update(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"))
	logger.Info("update workflow requested")
	existing, ok := a.load(req, resp, logger, rbac.ActionEdit)
	if !ok {
		return
	}
	body, ok := decodeSave(req, resp, logger)
	if !ok {
		return
	}
	// Steps are replaced wholesale rather than merged. A workflow is an
	// ordered list, and there is no sane merge of two orderings — a partial
	// update of position 3 in a list the client last saw differently would
	// silently rewire the wrong step.
	if body.Steps != nil {
		if msg, valid := a.validate(req, logger, saveRequest{
			Name:        firstNonEmpty(body.Name, existing.Name),
			InputSchema: firstNonEmptyRaw(body.InputSchema, existing.InputSchema),
			Steps:       body.Steps,
		}); !valid {
			writeError(req, resp, http.StatusBadRequest, msg)
			return
		}
		existing.Steps = withStepIDs(body.Steps)
	}
	if name := strings.TrimSpace(body.Name); name != "" {
		existing.Name = name
	}
	// Provided at all means provided: an empty one clears the description
	// rather than being ignored.
	if body.Description != nil {
		existing.Description = body.description()
	}
	if body.InputSchema != nil {
		// {} clears it, as with an agent's output_schema: null is
		// indistinguishable from an absent field, and a schema constraining
		// nothing is indistinguishable from having none.
		if isEmptyJSONObject(body.InputSchema) {
			existing.InputSchema = nil
		} else {
			existing.InputSchema = body.InputSchema
		}
	}
	existing.UpdatedAt = time.Now().UTC()
	if err := a.repo.Update(req.Request.Context(), existing); err != nil {
		logger.Error("failed to update workflow", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to update the workflow")
		return
	}
	logger.Info("workflow updated", "steps", len(existing.Steps))
	requesthelper.WriteJSON(req, resp, http.StatusOK, existing)
}

func (a *WorkflowAPI) delete(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"))
	logger.Info("delete workflow requested")
	workflow, ok := a.load(req, resp, logger, rbac.ActionDelete)
	if !ok {
		return
	}
	if err := a.repo.Delete(req.Request.Context(), workflow.ID); err != nil {
		logger.Error("failed to delete workflow", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete the workflow")
		return
	}
	// Cascade: an execution history without its workflow is unreadable, and
	// leaving it behind would let a later workflow with the same id inherit
	// somebody else's runs.
	if err := a.executions.DeleteByWorkflow(req.Request.Context(), workflow.ID); err != nil {
		logger.Error("failed to delete workflow executions", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to delete the workflow's executions")
		return
	}
	// The files of every run, and anything a run chose to keep. Logged rather
	// than surfaced: the workflow record is already gone, so a retry stops at
	// the permission check above and never reaches this line — a 500 here
	// would report a failure the caller cannot act on. What is left behind is
	// unreachable, since the only references to it lived in the executions
	// just deleted, so this is wasted disk for an operator to notice rather
	// than data anyone can still read.
	if a.files != nil {
		if err := a.files.DeleteWorkflow(req.Request.Context(), workflow.ID); err != nil {
			logger.Error("failed to delete the workflow's files; they are now unreachable and must be swept",
				"err", err, "workflow_id", workflow.ID)
		}
	}
	logger.Info("workflow deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *WorkflowAPI) trigger(req *restful.Request, resp *restful.Response) {
	userID := identity.FromContext(req.Request.Context())
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"), "user_id", userID)
	logger.Info("workflow execution requested")

	// ActionUse, not ActionView: reading a chain of agent steps is not the
	// same as being allowed to spend model calls running it.
	workflow, ok := a.load(req, resp, logger, rbac.ActionUse)
	if !ok {
		return
	}

	var body triggerRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && err.Error() != "EOF" {
		logger.Warn("invalid trigger body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if len(body.Input) > maxInputBytes {
		writeError(req, resp, http.StatusBadRequest,
			fmt.Sprintf("input is %d bytes; the limit is %d", len(body.Input), maxInputBytes))
		return
	}
	// Checked before anything is queued, so a malformed payload is refused
	// here rather than failing three steps and several model calls later.
	if err := workflows.ValidateAgainst(workflow.InputSchema, body.Input); err != nil {
		logger.Warn("trigger input does not match the workflow's input schema", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("input does not match this workflow's input schema: %s", err))
		return
	}

	now := time.Now().UTC()
	execution := workflows.Execution{
		ID:         uuid.NewString(),
		WorkflowID: workflow.ID,
		// From the authenticated caller, never from the body: this field is
		// the whole of the runner's authority, and every step will act as it.
		UserID:         userID,
		OrganizationID: workflow.OrganizationID,
		Status:         workflows.StatusQueued,
		Input:          body.Input,
		// The definition is copied onto the record. The workflow can be edited
		// while this runs, or long before anyone reads the result back; without
		// the copy the run would be explained by steps it never executed.
		Steps:    workflow.Steps,
		QueuedAt: now,
	}
	if err := a.executions.Save(req.Request.Context(), execution); err != nil {
		logger.Error("failed to create the execution record", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to queue the execution")
		return
	}
	// Published after the record exists, so the runner cannot be handed an id
	// it is unable to read.
	if err := a.producer.PublishExecution(req.Request.Context(),
		queue.ExecutionMessage{ExecutionID: execution.ID}); err != nil {
		logger.Error("failed to enqueue the execution", "execution_id", execution.ID, "err", err)
		// The record exists but nothing will run it. Say so on the record
		// rather than leaving it "queued" forever.
		execution.Status = workflows.StatusFailed
		execution.Error = "the execution could not be queued"
		execution.FinishedAt = time.Now().UTC()
		_ = a.executions.Save(req.Request.Context(), execution)
		writeError(req, resp, http.StatusBadGateway, "failed to queue the execution")
		return
	}
	logger.Info("workflow execution queued", "execution_id", execution.ID, "steps", len(execution.Steps))
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, execution)
}

// shapes resolves what every step produces and receives.
//
// The agents are fetched here rather than in the domain package so the
// resolution rules stay a pure function; and they are fetched AS THE CALLER,
// so a workflow referencing an agent this user cannot see resolves to prose
// rather than leaking that agent's schema.
func (a *WorkflowAPI) shapes(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"))
	logger.Info("workflow shapes requested")
	workflow, ok := a.load(req, resp, logger, rbac.ActionView)
	if !ok {
		return
	}

	// One call per DISTINCT agent, not per step: a workflow that uses the
	// same agent three times should cost one lookup.
	agentSchemas := map[string]json.RawMessage{}
	for _, agentID := range workflows.AgentIDs(workflow.Steps) {
		agent, found, err := a.agents.Get(req.Request.Context(), agentID)
		if err != nil {
			// A shapes response is an editor aid, not the workflow itself. An
			// agent that cannot be read degrades that one step to prose rather
			// than failing the whole response.
			logger.Warn("could not resolve an agent's output schema", "agent_id", agentID, "err", err)
			continue
		}
		if found && len(agent.OutputSchema) > 0 {
			agentSchemas[agentID] = agent.OutputSchema
		}
	}

	resolved, err := workflows.ResolveShapes(workflow, agentSchemas)
	if err != nil {
		logger.Error("failed to resolve shapes", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to resolve the workflow's shapes")
		return
	}
	logger.Info("workflow shapes resolved", "steps", len(resolved.Steps), "agents_resolved", len(agentSchemas))
	requesthelper.WriteJSON(req, resp, http.StatusOK, resolved)
}

func (a *WorkflowAPI) listExecutions(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger).WithFields("workflow_id", req.PathParameter("id"))
	logger.Info("list executions requested")
	workflow, ok := a.load(req, resp, logger, rbac.ActionView)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(req.QueryParameter("limit"))
	records, err := a.executions.ListByWorkflow(req.Request.Context(), workflow.OrganizationID, workflow.ID, limit)
	if err != nil {
		logger.Error("failed to list executions", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to list executions")
		return
	}
	logger.Info("executions listed", "count", len(records))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listExecutionsResponse{Executions: records})
}

func (a *WorkflowAPI) getExecution(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("execution_id", id)
	logger.Info("get execution requested")

	organizationID, ok := a.organization(req, resp, "execution not found")
	if !ok {
		return
	}
	execution, found, err := a.executions.Get(req.Request.Context(), organizationID, id)
	if err != nil {
		logger.Error("failed to load execution", "err", err)
		writeError(req, resp, http.StatusInternalServerError, "failed to load the execution")
		return
	}
	if !found {
		logger.Warn("execution not found")
		writeError(req, resp, http.StatusNotFound, "execution not found")
		return
	}
	// Permission is checked against the WORKFLOW, which is what roles are
	// granted on — an execution has no rules of its own, and inventing some
	// would leave two places to keep in agreement.
	if err := a.enforcer.RequireResource(req.Request.Context(),
		rbac.ResourceWorkflow, rbac.ActionView, execution.WorkflowID); err != nil {
		logger.Warn("execution access denied", "workflow_id", execution.WorkflowID, "err", err)
		rbac.WriteDenied(req, resp, err, "execution not found")
		return
	}
	logger.Info("execution fetched", "status", execution.Status, "runs", len(execution.Runs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, execution)
}

// withStepIDs assigns an id to any step that arrived without one, so a step
// keeps a stable identity across edits even when it is renamed or moved.
func withStepIDs(steps []workflows.Step) []workflows.Step {
	out := make([]workflows.Step, 0, len(steps))
	for _, step := range steps {
		if step.ID == "" {
			step.ID = uuid.NewString()
		}
		out = append(out, step)
	}
	return out
}

// isEmptyJSONObject reports whether raw is {} — the sentinel that clears a
// schema on a partial update.
func isEmptyJSONObject(raw json.RawMessage) bool {
	var doc map[string]json.RawMessage
	return json.Unmarshal(raw, &doc) == nil && len(doc) == 0
}

// firstNonEmptyRaw picks the incoming schema when one was supplied, else the
// stored one — so validating an update does not reject a workflow because the
// request omitted a schema it already has.
func firstNonEmptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(v) > 0 && !isEmptyJSONObject(v) {
			return v
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func decodeSave(req *restful.Request, resp *restful.Response, logger *logging.Logger) (saveRequest, bool) {
	var body saveRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid request body", "err", err)
		writeError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return saveRequest{}, false
	}
	return body, true
}

func writeError(req *restful.Request, resp *restful.Response, status int, msg string) {
	requesthelper.WriteError(req, resp, status, msg)
}
