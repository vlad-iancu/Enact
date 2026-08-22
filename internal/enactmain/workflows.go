package enactmain

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/rbac"
	"enact/internal/requesthelper"
	"enact/internal/workflows"
)

// workflowListItem is a workflow plus what the caller may do with it. The
// workflow's own fields are embedded, so the JSON is the workflow object with
// three extra keys rather than a wrapper the UI has to unpack.
type workflowListItem struct {
	workflows.Workflow
	resourceFlags
}

type listWorkflowsResponse struct {
	Workflows []workflowListItem `json:"workflows"`
}

type listWorkflowExecutionsResponse struct {
	WorkflowID string                `json:"workflow_id"`
	Executions []workflows.Execution `json:"executions"`
}

// workflowsWebService returns the workflow routes.
//
// requireCaller, not requireSession: manual triggering is the whole point of
// this feature in v1, and the thing doing the triggering is a script or a
// workflow tool holding an API key, not a browser.
func (a *MainAPI) workflowsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/workflows").Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	ws.Filter(a.requireCaller)

	ws.Route(ws.GET("").
		To(a.listWorkflows).
		Doc("List the workflows the caller may see").
		Returns(http.StatusOK, "OK", listWorkflowsResponse{}))

	ws.Route(ws.POST("").
		To(a.createWorkflow).
		Consumes(restful.MIME_JSON).
		Reads(workflows.SaveRequest{}).
		Doc("Create a workflow").
		Returns(http.StatusCreated, "Created", workflows.Workflow{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.getWorkflow).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Get a workflow").
		Returns(http.StatusOK, "OK", workflows.Workflow{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateWorkflow).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Update a workflow").
		Returns(http.StatusOK, "OK", workflows.Workflow{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteWorkflow).
		Param(ws.PathParameter("id", "workflow id")).
		Doc("Delete a workflow and its execution history").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/executions").
		To(a.triggerWorkflow).
		Consumes(restful.MIME_JSON).
		Param(ws.PathParameter("id", "workflow id")).
		Reads(workflows.TriggerRequest{}).
		Doc("Run a workflow. Returns immediately; poll the execution for progress").
		Returns(http.StatusAccepted, "Queued", workflows.Execution{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}/executions").
		To(a.listWorkflowExecutions).
		Param(ws.PathParameter("id", "workflow id")).
		Param(ws.QueryParameter("limit", "how many to return (default 50, max 200)").DataType("integer")).
		Doc("List a workflow's executions, newest first").
		Returns(http.StatusOK, "OK", listWorkflowExecutionsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/executions/{executionId}").
		To(a.getWorkflowExecution).
		Param(ws.PathParameter("executionId", "execution id")).
		Doc("Get one execution, with what every step received and produced").
		Returns(http.StatusOK, "OK", workflows.Execution{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

func (a *MainAPI) listWorkflows(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("list workflows requested")
	list, err := a.workflows.List(req.Request.Context())
	if err != nil {
		logger.Error("failed to list workflows", "err", err)
		relayAgentErr(req, resp, err, "list workflows")
		return
	}
	// One rules lookup for the whole page; the flags are then decided locally
	// with the same matcher the services enforce with.
	effective := a.effectiveFor(req, logger, sess.UserID)
	out := make([]workflowListItem, 0, len(list))
	runnable := 0
	for _, w := range list {
		item := workflowListItem{Workflow: w, resourceFlags: flagsFor(effective, rbac.ResourceWorkflow, w.ID)}
		if item.Usable {
			runnable++
		}
		out = append(out, item)
	}
	logger.Info("workflows listed", "count", len(out), "runnable", runnable)
	requesthelper.WriteJSON(req, resp, http.StatusOK, listWorkflowsResponse{Workflows: out})
}

func (a *MainAPI) createWorkflow(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create workflow requested")

	var body workflows.SaveRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create workflow body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	created, err := a.workflows.Create(req.Request.Context(), body)
	if err != nil {
		logger.Warn("workflow creation failed", "err", err)
		relayAgentErr(req, resp, err, "create workflow")
		return
	}
	logger.Info("workflow created", "workflow_id", created.ID, "steps", len(created.Steps))
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) getWorkflow(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "workflow_id", id)
	logger.Info("get workflow requested")

	workflow, found, err := a.workflows.Get(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to get workflow", "err", err)
		relayAgentErr(req, resp, err, "get workflow")
		return
	}
	if !found {
		logger.Warn("workflow not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, "workflow not found")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	item := workflowListItem{Workflow: workflow, resourceFlags: flagsFor(effective, rbac.ResourceWorkflow, workflow.ID)}
	logger.Info("workflow fetched", "name", workflow.Name, "usable", item.Usable)
	requesthelper.WriteJSON(req, resp, http.StatusOK, item)
}

func (a *MainAPI) updateWorkflow(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "workflow_id", id)
	logger.Info("update workflow requested")

	// Raw passthrough preserves the workflow service's update semantics end
	// to end.
	rawBody, err := io.ReadAll(req.Request.Body)
	if err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
		return
	}
	updated, found, err := a.workflows.Update(req.Request.Context(), id, rawBody)
	if err != nil {
		logger.Warn("workflow update failed", "err", err)
		relayAgentErr(req, resp, err, "update workflow")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "workflow not found")
		return
	}
	logger.Info("workflow updated")
	requesthelper.WriteJSON(req, resp, http.StatusOK, updated)
}

func (a *MainAPI) deleteWorkflow(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "workflow_id", id)
	logger.Info("delete workflow requested")

	found, err := a.workflows.Delete(req.Request.Context(), id)
	if err != nil {
		logger.Error("workflow deletion failed", "err", err)
		relayAgentErr(req, resp, err, "delete workflow")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "workflow not found")
		return
	}
	logger.Info("workflow deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) triggerWorkflow(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "workflow_id", id)
	logger.Info("workflow run requested")

	var body workflows.TriggerRequest
	// An empty body is ordinary: a workflow whose first step needs no input is
	// triggered with nothing at all.
	if req.Request.Body != nil {
		raw, err := io.ReadAll(req.Request.Body)
		if err != nil {
			requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
			return
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				requesthelper.WriteError(req, resp, http.StatusBadRequest, "invalid request body: "+err.Error())
				return
			}
		}
	}
	execution, found, err := a.workflows.Trigger(req.Request.Context(), id, body)
	if err != nil {
		logger.Warn("workflow run failed to queue", "err", err)
		relayAgentErr(req, resp, err, "run workflow")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "workflow not found")
		return
	}
	logger.Info("workflow run queued", "execution_id", execution.ID)
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, execution)
}

func (a *MainAPI) listWorkflowExecutions(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "workflow_id", id)
	logger.Info("list workflow executions requested")

	limit, _ := strconv.Atoi(req.QueryParameter("limit"))
	list, found, err := a.workflows.ListExecutions(req.Request.Context(), id, limit)
	if err != nil {
		logger.Error("failed to list executions", "err", err)
		relayAgentErr(req, resp, err, "list executions")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "workflow not found")
		return
	}
	logger.Info("workflow executions listed", "count", len(list))
	requesthelper.WriteJSON(req, resp, http.StatusOK,
		listWorkflowExecutionsResponse{WorkflowID: id, Executions: list})
}

func (a *MainAPI) getWorkflowExecution(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("executionId")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "execution_id", id)
	logger.Info("get workflow execution requested")

	execution, found, err := a.workflows.GetExecution(req.Request.Context(), id)
	if err != nil {
		logger.Error("failed to get execution", "err", err)
		relayAgentErr(req, resp, err, "get execution")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "execution not found")
		return
	}
	logger.Info("execution fetched", "status", execution.Status, "runs", len(execution.Runs))
	requesthelper.WriteJSON(req, resp, http.StatusOK, execution)
}
