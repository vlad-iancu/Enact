package enactmain

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	restful "github.com/emicklei/go-restful/v3"

	"enact/internal/crawls"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// crawlListItem is a crawl plus what the caller may do with it. The crawl's
// own fields are embedded, so the JSON is the crawl object with three extra
// keys rather than a wrapper the UI has to unpack.
type crawlListItem struct {
	crawls.Crawl
	resourceFlags
}

type listCrawlsResponse struct {
	Crawls []crawlListItem `json:"crawls"`
}

type listCrawlRunsResponse struct {
	CrawlID string       `json:"crawl_id"`
	Runs    []crawls.Run `json:"runs"`
}

// crawlsWebService returns the session-guarded crawl routes: the UI's window
// onto the crawl service, proxied caller-for-caller so the session user's
// permissions apply downstream.
func (a *MainAPI) crawlsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/crawls").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	ws.Filter(a.csrfOriginFilter)
	// requireCaller, not requireSession: a crawl is exactly the kind of thing
	// a program triggers, so an API key must be able to reach these.
	ws.Filter(a.requireCaller)

	ws.Route(ws.POST("").
		To(a.createCrawl).
		Reads(crawls.SaveRequest{}).
		Doc("Create a focused crawl against an empty retrieval knowledge base").
		Returns(http.StatusCreated, "Created", crawls.Crawl{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("").
		To(a.listCrawls).
		Doc("List the caller's crawls").
		Returns(http.StatusOK, "OK", listCrawlsResponse{}))

	ws.Route(ws.GET("/{id}").
		To(a.getCrawl).
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Get a crawl").
		Returns(http.StatusOK, "OK", crawls.Crawl{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/{id}").
		To(a.updateCrawl).
		Param(ws.PathParameter("id", "crawl id")).
		Reads(crawls.SaveRequest{}).
		Doc("Update a crawl: query, seed URLs, bounds and schedule are editable").
		Returns(http.StatusOK, "OK", crawls.Crawl{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/{id}").
		To(a.deleteCrawl).
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Delete a crawl and its run history; the knowledge base is left alone").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/{id}/runs").
		To(a.triggerCrawl).
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Queue a run now; crawling is asynchronous").
		Returns(http.StatusAccepted, "Queued", crawls.Run{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/{id}/runs").
		To(a.listCrawlRuns).
		Param(ws.PathParameter("id", "crawl id")).
		Param(ws.QueryParameter("limit", "how many runs to return").DataType("integer")).
		Doc("List a crawl's runs, newest first").
		Returns(http.StatusOK, "OK", listCrawlRunsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	// Flattened rather than nested under the crawl, matching how workflow
	// executions are exposed: a run id is unique on its own.
	ws.Route(ws.GET("/runs/{runId}").
		To(a.getCrawlRun).
		Param(ws.PathParameter("runId", "run id")).
		Doc("Get one run's report: the disambiguated query, its expansion, and the crawl graph").
		Returns(http.StatusOK, "OK", crawls.Run{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

func (a *MainAPI) createCrawl(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)
	logger.Info("create crawl requested")

	var body crawls.SaveRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid create crawl body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	created, err := a.crawls.Create(req.Request.Context(), body)
	if err != nil {
		logger.Warn("crawl creation failed", "err", err)
		relayAgentErr(req, resp, err, "create crawl")
		return
	}
	logger.Info("crawl created", "crawl_id", created.ID)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, created)
}

func (a *MainAPI) listCrawls(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID)

	records, err := a.crawls.List(req.Request.Context())
	if err != nil {
		logger.Error("failed to list crawls", "err", err)
		relayAgentErr(req, resp, err, "list crawls")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	out := make([]crawlListItem, 0, len(records))
	for _, c := range records {
		out = append(out, crawlListItem{
			Crawl:         c,
			resourceFlags: flagsFor(effective, rbac.ResourceCrawl, c.ID),
		})
	}
	logger.Info("crawls listed", "crawls", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK, listCrawlsResponse{Crawls: out})
}

func (a *MainAPI) getCrawl(req *restful.Request, resp *restful.Response) {
	sess := sessionAttr(req)
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", sess.UserID, "crawl_id", id)

	crawl, found, err := a.crawls.Get(req.Request.Context(), id)
	if err != nil {
		relayAgentErr(req, resp, err, "get crawl")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return
	}
	effective := a.effectiveFor(req, logger, sess.UserID)
	requesthelper.WriteJSON(req, resp, http.StatusOK, crawlListItem{
		Crawl:         crawl,
		resourceFlags: flagsFor(effective, rbac.ResourceCrawl, crawl.ID),
	})
}

func (a *MainAPI) updateCrawl(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", id)

	// Raw passthrough preserves the crawl service's update semantics end to
	// end.
	raw, err := io.ReadAll(req.Request.Body)
	if err != nil {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, "failed to read request body")
		return
	}
	updated, found, err := a.crawls.Update(req.Request.Context(), id, raw)
	if err != nil {
		logger.Warn("crawl update failed", "err", err)
		relayAgentErr(req, resp, err, "update crawl")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return
	}
	logger.Info("crawl updated")
	requesthelper.WriteJSON(req, resp, http.StatusOK, updated)
}

func (a *MainAPI) deleteCrawl(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", id)

	found, err := a.crawls.Delete(req.Request.Context(), id)
	if err != nil {
		relayAgentErr(req, resp, err, "delete crawl")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return
	}
	logger.Info("crawl deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *MainAPI) triggerCrawl(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", id)

	run, found, err := a.crawls.Trigger(req.Request.Context(), id)
	if err != nil {
		logger.Warn("crawl trigger failed", "err", err)
		relayAgentErr(req, resp, err, "run crawl")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return
	}
	logger.Info("crawl run queued", "run_id", run.ID)
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, run)
}

func (a *MainAPI) listCrawlRuns(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	limit := 0
	if raw := req.QueryParameter("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	runs, found, err := a.crawls.ListRuns(req.Request.Context(), id, limit)
	if err != nil {
		relayAgentErr(req, resp, err, "list crawl runs")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, listCrawlRunsResponse{CrawlID: id, Runs: runs})
}

func (a *MainAPI) getCrawlRun(req *restful.Request, resp *restful.Response) {
	runID := req.PathParameter("runId")
	run, found, err := a.crawls.GetRun(req.Request.Context(), runID)
	if err != nil {
		relayAgentErr(req, resp, err, "get crawl run")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "run not found")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, run)
}
