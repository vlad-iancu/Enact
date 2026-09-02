package enactcrawls

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"

	"enact/internal/crawls"
	"enact/internal/identity"
	"enact/internal/kb"
	"enact/internal/logging"
	"enact/internal/queue"
	"enact/internal/rbac"
	"enact/internal/requesthelper"
)

// CrawlAPI exposes crawl CRUD and run intake.
type CrawlAPI struct {
	repo     *crawls.Repository
	runs     *crawls.RunRepository
	kbs      *kb.Client
	producer *queue.Producer
	// rbac records who owns what; enforcer answers whether the caller may
	// act. Both are needed: creating a resource grants ownership of it, and
	// every other path checks.
	rbac     *rbac.Client
	enforcer *rbac.Enforcer
	// vault seals the credential headers a crawl carries. Sealing happens
	// here, on the way in, so nothing unsealed ever reaches the repository.
	vault  crawls.Sealer
	logger *logging.Logger
}

func newCrawlAPI(repo *crawls.Repository, runs *crawls.RunRepository, kbs *kb.Client,
	producer *queue.Producer, rbacClient *rbac.Client, enforcer *rbac.Enforcer,
	vault crawls.Sealer, logger *logging.Logger) *CrawlAPI {
	return &CrawlAPI{
		repo: repo, runs: runs, kbs: kbs, producer: producer,
		rbac: rbacClient, enforcer: enforcer, vault: vault, logger: logger,
	}
}

// saveRequest is the create/update body.
//
// Pointer fields on update distinguish "absent — leave unchanged" from
// "provided — set to this". The two fields a crawl exists to have edited,
// query and seed URLs, are both here.
type saveRequest struct {
	Name            string   `json:"name"`
	Query           string   `json:"query"`
	SeedURLs        []string `json:"seed_urls"`
	KnowledgeBaseID string   `json:"knowledge_base_id"`
	AllowedDomains  []string `json:"allowed_domains"`
	MaxPages        int      `json:"max_pages"`
	MaxDepth        int      `json:"max_depth"`
	MaxDurationSec  int      `json:"max_duration_seconds"`
	ScoreThreshold  float64  `json:"score_threshold"`
	Alpha           float64  `json:"alpha"`
	IntervalMinutes int      `json:"interval_minutes"`
	Enabled         *bool    `json:"enabled"`
	// ExtractionRules is nil-versus-empty sensitive on update: absent leaves
	// the rules alone, an empty array clears them. Without the pointer there
	// is no way to say "go back to letting the extractor decide".
	ExtractionRules *[]crawls.ExtractionRule `json:"extraction_rules"`
	// Credentials is write-only. Absent on update leaves the stored headers
	// alone — which is what makes it possible to edit a crawl's query without
	// re-sending its secrets — and an empty array clears them.
	Credentials *[]crawls.CredentialRule `json:"credentials"`
	// Source and JIRA select and configure the space explored. Absent on
	// update leaves them alone; the token, like any credential, is write-only.
	Source string             `json:"source"`
	JIRA   *crawls.JIRAConfig `json:"jira"`
}

type listCrawlsResponse struct {
	Crawls []crawls.Crawl `json:"crawls"`
}

type listRunsResponse struct {
	CrawlID string       `json:"crawl_id"`
	Runs    []crawls.Run `json:"runs"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// organization resolves the caller's organization for a scoped lookup,
// writing the refusal itself when they have none. Every read passes through
// it, so no path can accidentally fetch across the boundary.
func (a *CrawlAPI) organization(req *restful.Request, resp *restful.Response, notFound string) (string, bool) {
	organizationID, err := a.enforcer.Organization(req.Request.Context())
	if err != nil {
		rbac.WriteDenied(req, resp, err, notFound)
		return "", false
	}
	return organizationID, true
}

func (a *CrawlAPI) WebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/v1").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON)

	ws.Route(ws.POST("/crawls").
		To(a.create).
		Reads(saveRequest{}).
		Doc("Create a focused crawl. The knowledge base must be of kind \"rag\" and empty: the crawl becomes its sole writer.").
		Returns(http.StatusCreated, "Created", crawls.Crawl{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}))

	ws.Route(ws.GET("/crawls").
		To(a.list).
		Doc("List the caller's crawls").
		Returns(http.StatusOK, "OK", listCrawlsResponse{}))

	ws.Route(ws.GET("/crawls/{id}").
		To(a.get).
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Get a crawl").
		Returns(http.StatusOK, "OK", crawls.Crawl{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.PUT("/crawls/{id}").
		To(a.update).
		Param(ws.PathParameter("id", "crawl id")).
		Reads(saveRequest{}).
		Doc("Update a crawl: the query, seed URLs, bounds and schedule are all editable; the knowledge base is not").
		Returns(http.StatusOK, "OK", crawls.Crawl{}).
		Returns(http.StatusBadRequest, "Invalid request", errorResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.DELETE("/crawls/{id}").
		To(a.delete).
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Delete a crawl and its run history. The knowledge base it filled is left alone.").
		Returns(http.StatusNoContent, "Deleted", nil).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.POST("/crawls/{id}/runs").
		To(a.trigger).
		// Takes no body, so it must not demand a JSON content type: a bare
		// `curl -X POST` is the obvious way to start a run by hand, and the
		// WebService-level Consumes would answer it with 415.
		Consumes("*/*").
		Param(ws.PathParameter("id", "crawl id")).
		Doc("Queue a run now; crawling is asynchronous").
		Returns(http.StatusAccepted, "Queued", crawls.Run{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/crawls/{id}/runs").
		To(a.listRuns).
		Param(ws.PathParameter("id", "crawl id")).
		Param(ws.QueryParameter("limit", "how many runs to return (default 50, max 200)").DataType("integer")).
		Doc("List a crawl's runs, newest first").
		Returns(http.StatusOK, "OK", listRunsResponse{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	ws.Route(ws.GET("/runs/{id}").
		To(a.getRun).
		Param(ws.PathParameter("id", "run id")).
		Doc("Get one run's full report: the disambiguated and expanded query, and the crawl graph").
		Returns(http.StatusOK, "OK", crawls.Run{}).
		Returns(http.StatusNotFound, "Not found", errorResponse{}))

	return ws
}

// load is the gate every single-crawl route passes through, so the permission
// check and the organization scoping cannot drift apart.
func (a *CrawlAPI) load(req *restful.Request, resp *restful.Response, action string) (crawls.Crawl, bool) {
	id := req.PathParameter("id")
	ctx := req.Request.Context()
	if err := a.enforcer.RequireResource(ctx, rbac.ResourceCrawl, action, id); err != nil {
		rbac.WriteDenied(req, resp, err, "crawl not found")
		return crawls.Crawl{}, false
	}
	organizationID, ok := a.organization(req, resp, "crawl not found")
	if !ok {
		return crawls.Crawl{}, false
	}
	crawl, found, err := a.repo.Get(ctx, organizationID, id)
	if err != nil {
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to look up crawl")
		return crawls.Crawl{}, false
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "crawl not found")
		return crawls.Crawl{}, false
	}
	return crawl, true
}

func decode(req *restful.Request, resp *restful.Response, logger *logging.Logger) (saveRequest, bool) {
	var body saveRequest
	dec := json.NewDecoder(req.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		logger.Warn("invalid request body", "err", err)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return saveRequest{}, false
	}
	return body, true
}

func (a *CrawlAPI) create(req *restful.Request, resp *restful.Response) {
	ctx := req.Request.Context()
	userID := identity.FromContext(ctx)
	logger := requesthelper.Logger(req, a.logger).WithFields("user_id", userID)
	logger.Info("create crawl requested")

	if err := a.enforcer.Require(ctx, rbac.Permission(rbac.ResourceCrawl, rbac.ActionCreate, "*")); err != nil {
		logger.Warn("create crawl denied", "err", err)
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	body, ok := decode(req, resp, logger)
	if !ok {
		return
	}
	organizationID, ok := a.organization(req, resp, "crawl not found")
	if !ok {
		return
	}

	now := time.Now().UTC()
	crawl := crawls.ApplyDefaults(crawls.Crawl{
		ID:              uuid.NewString(),
		UserID:          userID,
		OrganizationID:  organizationID,
		Name:            strings.TrimSpace(body.Name),
		Query:           strings.TrimSpace(body.Query),
		SeedURLs:        body.SeedURLs,
		ExtractionRules: rulesOf(body.ExtractionRules),
		Credentials:     credentialsOf(body.Credentials),
		Source:          body.Source,
		JIRA:            body.JIRA,
		KnowledgeBaseID: strings.TrimSpace(body.KnowledgeBaseID),
		AllowedDomains:  body.AllowedDomains,
		MaxPages:        body.MaxPages,
		MaxDepth:        body.MaxDepth,
		MaxDurationSec:  body.MaxDurationSec,
		ScoreThreshold:  body.ScoreThreshold,
		Alpha:           body.Alpha,
		IntervalMinutes: body.IntervalMinutes,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	crawl.Enabled = body.Enabled == nil || *body.Enabled
	// A scheduled crawl runs its first pass on the next sweep rather than
	// waiting out a whole interval: a user who creates a daily crawl expects
	// something to happen today.
	if crawl.Enabled && crawl.Interval() > 0 {
		crawl.NextRunAt = now
	}
	if msg, valid := crawls.Validate(crawl); !valid {
		logger.Warn("crawl validation failed", "err", msg)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, msg)
		return
	}
	if msg, valid := a.validateKnowledgeBase(ctx, logger, crawl.KnowledgeBaseID); !valid {
		requesthelper.WriteError(req, resp, http.StatusBadRequest, msg)
		return
	}

	if err := crawls.SealCrawl(a.vault, &crawl); err != nil {
		logger.Error("failed to seal crawl credentials", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "could not store the credentials")
		return
	}
	if err := a.repo.Create(ctx, crawl); err != nil {
		logger.Error("failed to create crawl", "crawl_id", crawl.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to create crawl")
		return
	}
	// The creator owns it. Recorded before replying, so a client that reads
	// the crawl back immediately can.
	if err := a.rbac.Grant(ctx, rbac.GrantRequest{
		UserID: userID, Resource: rbac.ResourceCrawl, ResourceID: crawl.ID,
	}); err != nil {
		logger.Error("failed to record ownership", "crawl_id", crawl.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to record ownership of the crawl")
		return
	}
	// The caller's cached rules predate this grant; without dropping them
	// they would be refused the crawl they just created until the TTL expired.
	a.enforcer.Forget(userID)

	logger.Info("crawl created", "crawl_id", crawl.ID, "name", crawl.Name,
		"kb_id", crawl.KnowledgeBaseID, "seeds", len(crawl.SeedURLs),
		"interval_minutes", crawl.IntervalMinutes)
	requesthelper.WriteJSON(req, resp, http.StatusCreated, crawl.Redacted())
}

// validateKnowledgeBase checks that the crawl's target is a retrieval
// knowledge base with nothing in it.
//
// Both halves matter. A context knowledge base stores documents whole and has
// no embeddings, so a crawl filling one would produce a corpus nothing
// retrieves from. And a crawl OWNS its knowledge base: it keeps a
// URL-to-document map so a repeat run can replace what changed, which is only
// coherent if nothing else put documents there. Requiring it empty at
// creation is how that ownership is established — the alternative is a crawl
// that silently deletes somebody else's uploads on its second run.
//
// The lookup goes through the KB service, so the caller's own permissions
// apply: a knowledge base they cannot see is reported as not found.
//
// The emptiness half is eventually consistent, and knowingly so. Uploads are
// queued, extracted and embedded asynchronously, so a document uploaded
// seconds ago is not yet visible here and a crawl created in that window is
// accepted. Closing the window would need a synchronous count the KB API does
// not offer. The residual risk is small and bounded — somebody would have to
// upload to a knowledge base and point a crawl at it within the same few
// seconds — while the check still catches every case that matters: an
// established knowledge base with a corpus in it.
func (a *CrawlAPI) validateKnowledgeBase(ctx context.Context, logger *logging.Logger, kbID string) (string, bool) {
	record, found, err := a.kbs.Get(ctx, kbID)
	if err != nil {
		logger.Error("failed to validate knowledge base", "kb_id", kbID, "err", err)
		return "failed to validate the knowledge base", false
	}
	if !found {
		logger.Warn("crawl validation failed: knowledge base not found", "kb_id", kbID)
		return fmt.Sprintf("knowledge base %q not found", kbID), false
	}
	if kind := kb.NormalizeKind(record.Kind); kind != kb.KindRetrieval {
		logger.Warn("crawl validation failed: knowledge base is not a retrieval one", "kb_id", kbID, "kind", kind)
		return fmt.Sprintf("knowledge base %q is a %s knowledge base; a crawl needs one of kind %q",
			kbID, kind, kb.KindRetrieval), false
	}
	documents, _, err := a.kbs.ListDocuments(ctx, kbID)
	if err != nil {
		logger.Error("failed to check whether the knowledge base is empty", "kb_id", kbID, "err", err)
		return "failed to check whether the knowledge base is empty", false
	}
	if len(documents) > 0 {
		logger.Warn("crawl validation failed: knowledge base is not empty", "kb_id", kbID, "documents", len(documents))
		return fmt.Sprintf("knowledge base %q already holds %d documents; a crawl needs an empty one, "+
			"because it manages that knowledge base's contents itself", kbID, len(documents)), false
	}
	return "", true
}

func (a *CrawlAPI) list(req *restful.Request, resp *restful.Response) {
	ctx := req.Request.Context()
	logger := requesthelper.Logger(req, a.logger)
	logger.Info("list crawls requested")

	// Candidates are the organization's; the caller's rules decide which they
	// may see. A user with no roles still gets their own, through the hidden
	// ownership role rather than a hard-coded owner filter.
	organizationID, err := a.enforcer.Organization(ctx)
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	effective, err := a.enforcer.CallerEffective(ctx)
	if err != nil {
		rbac.WriteDeniedForbidden(req, resp, err)
		return
	}
	records, err := a.repo.List(ctx, organizationID)
	if err != nil {
		logger.Error("failed to list crawls", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list crawls")
		return
	}
	out := make([]crawls.Crawl, 0, len(records))
	for _, c := range records {
		if !effective.Allows(rbac.Permission(rbac.ResourceCrawl, rbac.ActionView, c.ID)) {
			continue
		}
		out = append(out, c)
	}
	logger.Info("crawls listed", "candidates", len(records), "visible", len(out))
	requesthelper.WriteJSON(req, resp, http.StatusOK,
		listCrawlsResponse{Crawls: crawls.RedactAll(out)})
}

func (a *CrawlAPI) get(req *restful.Request, resp *restful.Response) {
	crawl, ok := a.load(req, resp, rbac.ActionView)
	if !ok {
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, crawl.Redacted())
}

func (a *CrawlAPI) update(req *restful.Request, resp *restful.Response) {
	crawl, ok := a.load(req, resp, rbac.ActionEdit)
	if !ok {
		return
	}
	ctx := req.Request.Context()
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", crawl.ID)
	body, ok := decode(req, resp, logger)
	if !ok {
		return
	}
	// The knowledge base is deliberately not updatable: the crawl holds a
	// URL-to-document map against it, and repointing the crawl would leave
	// those documents orphaned in the old knowledge base and the map lying
	// about the new one.
	if kbID := strings.TrimSpace(body.KnowledgeBaseID); kbID != "" && kbID != crawl.KnowledgeBaseID {
		requesthelper.WriteError(req, resp, http.StatusBadRequest,
			"knowledge_base_id cannot be changed; create a new crawl against the other knowledge base")
		return
	}

	if name := strings.TrimSpace(body.Name); name != "" {
		crawl.Name = name
	}
	if query := strings.TrimSpace(body.Query); query != "" {
		crawl.Query = query
	}
	if len(body.SeedURLs) > 0 {
		crawl.SeedURLs = body.SeedURLs
	}
	if body.ExtractionRules != nil {
		crawl.ExtractionRules = *body.ExtractionRules
	}
	if body.Credentials != nil {
		crawl.Credentials = *body.Credentials
	}
	if body.Source != "" {
		crawl.Source = body.Source
	}
	if body.JIRA != nil {
		crawl.JIRA = mergeJIRA(crawl.JIRA, body.JIRA)
	}
	if len(body.AllowedDomains) > 0 {
		crawl.AllowedDomains = body.AllowedDomains
	}
	if body.MaxPages > 0 {
		crawl.MaxPages = body.MaxPages
	}
	if body.MaxDepth > 0 {
		crawl.MaxDepth = body.MaxDepth
	}
	if body.MaxDurationSec > 0 {
		crawl.MaxDurationSec = body.MaxDurationSec
	}
	if body.ScoreThreshold > 0 {
		crawl.ScoreThreshold = body.ScoreThreshold
	}
	if body.Alpha > 0 {
		crawl.Alpha = body.Alpha
	}
	crawl.IntervalMinutes = body.IntervalMinutes
	if body.Enabled != nil {
		crawl.Enabled = *body.Enabled
	}
	crawl.UpdatedAt = time.Now().UTC()
	// Re-schedule from now, so shortening an interval takes effect
	// immediately rather than after the old one elapses.
	if crawl.Enabled && crawl.Interval() > 0 {
		crawl.NextRunAt = time.Now().UTC()
	}

	if msg, valid := crawls.Validate(crawl); !valid {
		logger.Warn("crawl update validation failed", "err", msg)
		requesthelper.WriteError(req, resp, http.StatusBadRequest, msg)
		return
	}
	if err := crawls.SealCrawl(a.vault, &crawl); err != nil {
		logger.Error("failed to seal crawl credentials", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "could not store the credentials")
		return
	}
	if err := a.repo.Update(ctx, crawl); err != nil {
		logger.Error("failed to update crawl", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to update crawl")
		return
	}
	logger.Info("crawl updated", "query_chars", len(crawl.Query), "seeds", len(crawl.SeedURLs),
		"interval_minutes", crawl.IntervalMinutes, "enabled", crawl.Enabled)
	requesthelper.WriteJSON(req, resp, http.StatusOK, crawl.Redacted())
}

func (a *CrawlAPI) delete(req *restful.Request, resp *restful.Response) {
	crawl, ok := a.load(req, resp, rbac.ActionDelete)
	if !ok {
		return
	}
	ctx := req.Request.Context()
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", crawl.ID)

	if err := a.repo.Delete(ctx, crawl.ID); err != nil {
		logger.Error("failed to delete crawl", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to delete crawl")
		return
	}
	// Cascade to the run history: the reports describe a crawl that no longer
	// exists and nothing can reach them.
	if err := a.runs.DeleteByCrawl(ctx, crawl.ID); err != nil {
		// The crawl record is already gone, so a retry cannot reach this
		// point. Logged rather than surfaced, like the workflow file cascade.
		logger.Error("failed to delete crawl runs", "err", err)
	}
	// The knowledge base is NOT deleted. The crawl filled it, but the corpus
	// outlives the instruction that gathered it, and an agent may be using it.
	logger.Info("crawl deleted")
	resp.WriteHeader(http.StatusNoContent)
}

func (a *CrawlAPI) trigger(req *restful.Request, resp *restful.Response) {
	// "use" rather than "view": a run spends outbound requests against
	// third-party sites under the platform's name, and embedding cost for
	// every page it keeps.
	crawl, ok := a.load(req, resp, rbac.ActionUse)
	if !ok {
		return
	}
	ctx := req.Request.Context()
	logger := requesthelper.Logger(req, a.logger).WithFields("crawl_id", crawl.ID)

	run := crawls.Run{
		ID:      uuid.NewString(),
		CrawlID: crawl.ID,
		// The run acts as the CRAWL's owner, not as whoever triggered it:
		// the knowledge base being written belongs to the owner, and a
		// colleague with "use" must not cause writes under their own name.
		UserID:         crawl.UserID,
		OrganizationID: crawl.OrganizationID,
		Status:         crawls.StatusQueued,
		QueuedAt:       time.Now().UTC(),
	}
	// Written before publishing, so the orchestrator can never be handed an
	// id it cannot read.
	if err := a.runs.Save(ctx, run); err != nil {
		logger.Error("failed to record run", "run_id", run.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to queue the run")
		return
	}
	if err := a.producer.PublishCrawlRun(ctx, queue.CrawlRunMessage{RunID: run.ID}); err != nil {
		logger.Error("failed to publish run", "run_id", run.ID, "err", err)
		run.Status = crawls.StatusFailed
		run.Error = "the run could not be queued"
		run.FinishedAt = time.Now().UTC()
		if saveErr := a.runs.Save(ctx, run); saveErr != nil {
			logger.Error("failed to record the queueing failure", "run_id", run.ID, "err", saveErr)
		}
		requesthelper.WriteError(req, resp, http.StatusBadGateway, "failed to queue the run")
		return
	}
	logger.Info("crawl run queued", "run_id", run.ID)
	requesthelper.WriteJSON(req, resp, http.StatusAccepted, run)
}

func (a *CrawlAPI) listRuns(req *restful.Request, resp *restful.Response) {
	crawl, ok := a.load(req, resp, rbac.ActionView)
	if !ok {
		return
	}
	ctx := req.Request.Context()
	limit := 0
	if raw := req.QueryParameter("limit"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	runs, err := a.runs.ListByCrawl(ctx, crawl.OrganizationID, crawl.ID, limit)
	if err != nil {
		requesthelper.Logger(req, a.logger).Error("failed to list runs", "crawl_id", crawl.ID, "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to list runs")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, listRunsResponse{CrawlID: crawl.ID, Runs: runs})
}

func (a *CrawlAPI) getRun(req *restful.Request, resp *restful.Response) {
	id := req.PathParameter("id")
	ctx := req.Request.Context()
	logger := requesthelper.Logger(req, a.logger).WithFields("run_id", id)

	organizationID, ok := a.organization(req, resp, "run not found")
	if !ok {
		return
	}
	run, found, err := a.runs.Get(ctx, organizationID, id)
	if err != nil {
		logger.Error("failed to get run", "err", err)
		requesthelper.WriteError(req, resp, http.StatusInternalServerError, "failed to get run")
		return
	}
	if !found {
		requesthelper.WriteError(req, resp, http.StatusNotFound, "run not found")
		return
	}
	// A run has no rules of its own; permission is checked against the crawl
	// it belongs to.
	if err := a.enforcer.RequireResource(ctx, rbac.ResourceCrawl, rbac.ActionView, run.CrawlID); err != nil {
		logger.Warn("get run denied", "crawl_id", run.CrawlID, "err", err)
		rbac.WriteDenied(req, resp, err, "run not found")
		return
	}
	requesthelper.WriteJSON(req, resp, http.StatusOK, run)
}

// rulesOf dereferences the optional rule set, treating absent as none.
func rulesOf(rules *[]crawls.ExtractionRule) []crawls.ExtractionRule {
	if rules == nil {
		return nil
	}
	return *rules
}

// credentialsOf dereferences the optional credential set.
func credentialsOf(rules *[]crawls.CredentialRule) []crawls.CredentialRule {
	if rules == nil {
		return nil
	}
	return *rules
}

// mergeJIRA applies the fields an update actually sent, keeping the rest.
//
// Field-wise rather than wholesale because of what people do with this: the
// commonest edit by some distance is replacing an expired token, and
// `{"jira":{"token":"..."}}` is the obvious way to write that. Replacing the
// whole object would blank the base URL and the email and fail validation with
// a message about the base URL — which is not the thing that changed, and is a
// confusing answer to a correct request.
//
// The same reasoning in the other direction: an update that omits the token
// keeps the stored one, so a project list can be edited without handling
// secrets again.
func mergeJIRA(stored, update *crawls.JIRAConfig) *crawls.JIRAConfig {
	if stored == nil {
		return update
	}
	merged := *stored
	if update.BaseURL != "" {
		merged.BaseURL = update.BaseURL
	}
	if update.Email != "" {
		merged.Email = update.Email
	}
	if update.Token != "" {
		merged.Token = update.Token
	}
	if update.Projects != nil {
		merged.Projects = update.Projects
	}
	if update.MaxDepth != 0 {
		merged.MaxDepth = update.MaxDepth
	}
	return &merged
}
