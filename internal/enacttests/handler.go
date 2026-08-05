package enacttests

import (
	"encoding/json"
	"fmt"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/logging"
	"enact/internal/requesthelper"
)

// TestsAPI exposes asynchronous execution of the registered integration
// test cases against the live platform services.
type TestsAPI struct {
	runner *Runner
	logger *logging.Logger
}

func newTestsAPI(runner *Runner, logger *logging.Logger) *TestsAPI {
	return &TestsAPI{runner: runner, logger: logger}
}

type startExecutionRequest struct {
	// NumWorkers is the number of concurrent test workers (default 4).
	NumWorkers int `json:"num_workers,omitempty"`
	// Tests selects the cases to run by regex (default all).
	Tests string `json:"tests,omitempty"`
	// Skip excludes matching cases (default none).
	Skip string `json:"skip,omitempty"`
}

type startExecutionResponse struct {
	ExecID string `json:"exec_id"`
	// Selected reports how many cases matched, so a bad regex (0 selected)
	// is visible immediately rather than as an instantly-"done" execution.
	Selected int `json:"selected"`
}

type executionStatusResponse struct {
	Completed []caseResult `json:"completed"`
	Pending   int          `json:"pending"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (a *TestsAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1/execution").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("").
		To(a.start).
		Reads(startExecutionRequest{}).
		Doc("Launch an asynchronous integration-test execution; cases matching the tests regex run, those matching skip are excluded").
		Returns(http.StatusAccepted, "Execution started", startExecutionResponse{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.status).
		Param(ws.QueryParameter("id", "execution id returned by POST").Required(true)).
		Doc("Report an execution's completed test cases (with status) and the number still pending").
		Returns(http.StatusOK, "OK", executionStatusResponse{}).
		Returns(http.StatusBadRequest, "Missing id", errorResponse{}).
		Returns(http.StatusNotFound, "Unknown execution", errorResponse{}))

	return ws
}

func (a *TestsAPI) start(req *restful.Request, resp *restful.Response) {
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("test execution requested")

	var body startExecutionRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid execution request body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	logger.Info("execution request decoded", "num_workers", body.NumWorkers, "tests", body.Tests, "skip", body.Skip)

	execID, selected, err := a.runner.Start(body.Tests, body.Skip, body.NumWorkers)
	if err != nil {
		logger.Warn("failed to start execution", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, err.Error())
		return
	}
	logger.Info("execution launched", "exec_id", execID, "selected", selected)
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, startExecutionResponse{ExecID: execID, Selected: selected})
}

func (a *TestsAPI) status(req *restful.Request, resp *restful.Response) {
	id := req.QueryParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("exec_id", id)
	logger.Info("execution status requested")

	if id == "" {
		logger.Warn("execution status request without id")
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "query parameter `id` is required")
		return
	}
	exec, found := a.runner.Get(id)
	if !found {
		logger.Warn("execution not found")
		requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("execution %q not found", id))
		return
	}
	completed, pending := exec.snapshot()
	logger.Info("execution status reported", "completed", len(completed), "pending", pending)
	requesthelper.WriteJSON(req, resp, http.StatusOK, executionStatusResponse{Completed: completed, Pending: pending})
}
