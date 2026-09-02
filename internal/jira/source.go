// Package jira explores an issue tracker as a searchable space.
//
// A reference is an issue key — SCRUM-1, GENAI-1234 — retrieving it is a call
// to the Atlassian REST API, and the new references it yields are the issues
// that one points at: its subtasks, its children if it is an epic, and the
// issues it is linked to. That is the same shape as the web, which is why both
// satisfy source.Source and the crawl loop cannot tell them apart.
//
// The traversal was the open question. What an issue tracker offers is a graph
// with several kinds of edge, and the choice made here is to follow the ones
// that mean "part of the same piece of work" — subtask, epic child, and the
// explicit issue links a person took the trouble to create — and to ignore the
// ones that merely mean "somebody mentioned this", which pull a crawl across
// the whole backlog within two hops.
package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"enact/internal/logging"
	"enact/internal/netguard"
	"enact/internal/source"
)

// Config points at a site and says who is asking.
//
// Atlassian's REST API authenticates with HTTP Basic where the username is the
// account's email and the password is an API token. The token is a credential
// like any other: it arrives already unsealed and is never logged.
type Config struct {
	// BaseURL is the site: https://your-org.atlassian.net
	BaseURL string
	// Email is the account the token belongs to.
	Email string
	// Token is an Atlassian API token, unsealed.
	Token string
	// Projects restricts the crawl to these project keys. Empty means the
	// project of each seed, which is almost always what is wanted: a crawl
	// that wandered from SCRUM into every project on the site would be a
	// surprise and an expensive one.
	Projects []string
	// MaxDepth bounds how far the traversal follows relationships, counted
	// from the seed. The API makes each hop cheap, and cheap hops across a
	// graph with no diameter to speak of is how a crawl reads a whole backlog.
	MaxDepth int
	// Timeout bounds one API call.
	Timeout time.Duration
	// AllowPrivateAddresses disables the SSRF guard, for the test suite only.
	//
	// A base URL is user-supplied and fetched from the platform's own network
	// position, exactly like a crawl's seed — so it needs the same protection,
	// and it did not have it: this source was written with a plain http.Client
	// and would happily read 127.0.0.1 or the cloud metadata endpoint and file
	// the response in a knowledge base. Never set in a deployment.
	AllowPrivateAddresses bool

	// Logger records the things a retrieval survives rather than fails on —
	// an issue whose children could not be listed, a child search stopped at
	// its page cap. Optional; a nil logger discards.
	Logger *logging.Logger
}

// DefaultMaxDepth is how far issue relationships are followed.
//
// Two, which is further than it sounds: an epic, its children, and then their
// subtasks and links is already the whole of a normal piece of work. Issue
// graphs are dense — "relates to" is applied liberally and reciprocally — so
// each additional hop multiplies rather than adds.
const DefaultMaxDepth = 2

// issueKey is a project key, a hyphen, and a number: SCRUM-1, GENAI-1234.
var issueKey = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)-([0-9]+)$`)

// Source is the JIRA implementation of source.Source.
type Source struct {
	cfg      Config
	http     *http.Client
	auth     string
	projects map[string]bool

	// mu guards the fields below, which are resolved on first use.
	mu sync.Mutex
	// mode is which of Atlassian's two authentication schemes this token
	// wants. See resolveAuth.
	mode string
	// cloudID names the site on api.atlassian.com, needed by scoped tokens.
	cloudID string
	// apiHost is where scoped tokens are presented. A field so the test suite
	// can stand in for Atlassian; never configured in a deployment.
	apiHost string
}

// The two ways Atlassian accepts an API token.
const (
	// authBasic: a CLASSIC token, as HTTP Basic email:token against the site.
	authBasic = "basic"
	// authBearer: a SCOPED token, as a bearer credential against
	// api.atlassian.com/ex/jira/{cloudId}. Scoped tokens carry OAuth scopes
	// (read:jira-work and friends) and are simply not accepted by the site
	// host — a scoped token presented as Basic gets 401, indistinguishably
	// from a revoked one.
	authBearer = "bearer"
)

// New builds the source. It makes no request: a crawl that cannot reach its
// site should fail on the first retrieval, with that reference named, rather
// than at construction where there is nothing useful to attach the error to.
func New(cfg Config) (*Source, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("jira: base_url is required, as in https://your-org.atlassian.net")
	}
	u, err := url.Parse(base)
	if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("jira: base_url %q must be an absolute https URL", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Email) == "" || strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("jira: an account email and an API token are both required")
	}
	cfg.BaseURL = base
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	projects := map[string]bool{}
	for _, p := range cfg.Projects {
		projects[strings.ToUpper(strings.TrimSpace(p))] = true
	}
	return &Source{
		cfg: cfg,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{DialContext: netguard.Dialer(cfg.AllowPrivateAddresses)},
		},
		auth:     "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.Email+":"+cfg.Token)),
		projects: projects,
	}, nil
}

func (s *Source) Name() string    { return "jira" }
func (s *Source) Close() error    { s.http.CloseIdleConnections(); return nil }
func (s *Source) baseURL() string { return s.cfg.BaseURL }

// Parse accepts an issue key, or a browse URL containing one.
//
// Both, because a person copying a reference out of a browser has a URL and a
// person typing one has a key, and refusing either would be pedantry. The
// reference ID is always the browse URL: it is canonical, it is what a reader
// of the report wants to click, and it keeps every reference in the system a
// URL whether it came from the web or from here.
func (s *Source) Parse(seed string) (source.Reference, error) {
	key := strings.ToUpper(strings.TrimSpace(seed))
	if u, err := url.Parse(key); err == nil && u.IsAbs() {
		key = strings.ToUpper(strings.Trim(u.Path[strings.LastIndex(u.Path, "/")+1:], "/"))
	}
	if !issueKey.MatchString(key) {
		return source.Reference{}, fmt.Errorf(
			"%q is not an issue: expected a key like SCRUM-1, or a browse URL containing one", seed)
	}
	return source.Reference{ID: s.browseURL(key)}, nil
}

// Allows keeps the crawl inside its projects.
func (s *Source) Allows(ref source.Reference) bool {
	key := s.keyOf(ref)
	if key == "" {
		return false
	}
	if len(s.projects) == 0 {
		return true
	}
	return s.projects[projectOf(key)]
}

// SeedProjects returns the project keys a set of seeds belongs to, so a crawl
// that named no projects is still confined to the ones it started in.
func SeedProjects(seeds []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, seed := range seeds {
		key := strings.ToUpper(strings.TrimSpace(seed))
		if u, err := url.Parse(key); err == nil && u.IsAbs() {
			key = strings.ToUpper(strings.Trim(u.Path[strings.LastIndex(u.Path, "/")+1:], "/"))
		}
		if issueKey.MatchString(key) {
			if project := projectOf(key); !seen[project] {
				seen[project] = true
				out = append(out, project)
			}
		}
	}
	return out
}

func projectOf(key string) string {
	if m := issueKey.FindStringSubmatch(key); m != nil {
		return m[1]
	}
	return ""
}

func (s *Source) browseURL(key string) string { return s.cfg.BaseURL + "/browse/" + key }

// keyOf recovers the issue key from a reference ID.
func (s *Source) keyOf(ref source.Reference) string {
	id := ref.ID
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	id = strings.ToUpper(strings.TrimSpace(id))
	if !issueKey.MatchString(id) {
		return ""
	}
	return id
}

// Retrieve reads one issue and returns its text and the issues it points at.
func (s *Source) Retrieve(ctx context.Context, ref source.Reference) (source.Document, error) {
	key := s.keyOf(ref)
	if key == "" {
		return source.Document{}, fmt.Errorf("%w: %q is not an issue key", source.ErrNotFound, ref.ID)
	}
	if !s.Allows(ref) {
		return source.Document{}, fmt.Errorf("%w: %s is outside the crawl's projects", source.ErrOutOfScope, key)
	}

	// The field list is explicit rather than "*all": an issue carries a great
	// deal that is not text — permissions, worklogs, sprint metadata — and
	// asking for it costs time on every issue of every run.
	// Resolved lazily, so a Source used without Verify still authenticates
	// correctly rather than failing every issue as "not found".
	if _, err := s.resolveAuth(ctx); err != nil {
		return source.Document{}, err
	}
	endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s", s.apiBase(), key,
		"summary,description,issuetype,status,priority,labels,components,parent,subtasks,issuelinks,comment")

	var issue issueResponse
	if err := s.get(ctx, endpoint, &issue); err != nil {
		return source.Document{}, err
	}

	text := issueText(issue)
	if strings.TrimSpace(text) == "" {
		return source.Document{}, fmt.Errorf("%w: %s has no description or comments", source.ErrNotRetrievable, key)
	}

	refs := s.related(ctx, issue, ref.Depth)
	return source.Document{
		Title: fmt.Sprintf("%s %s", key, issue.Fields.Summary),
		Text:  text,
		// Scored deliberately excludes the field labels this package writes;
		// see issueProse.
		Scored:     issueProse(issue),
		References: refs,
	}, nil
}

// related is the traversal: which issues this one leads to.
//
// Subtasks and the parent because they are literally the same piece of work;
// explicit issue links because somebody chose to draw that edge and it is the
// only signal in a tracker that is deliberate. Mentions in text are not
// followed — an issue that names a dozen others in a comment would otherwise
// drag the crawl across the backlog, and a mention is not a relationship.
//
// The depth cut is enforced here rather than left to the crawl's own limit,
// because the two mean different things: the crawl's depth bounds the search,
// this one bounds how far a RELATIONSHIP is worth trusting, and issue graphs
// are dense enough that the second is reached long before the first.
func (s *Source) related(ctx context.Context, issue issueResponse, depth int) []source.Reference {
	if depth >= s.cfg.MaxDepth {
		return nil
	}
	var refs []source.Reference
	seen := map[string]bool{}
	// structural says the issue at the other end is part of the same piece of
	// work — a parent, a subtask, an epic's child. Those are followed on the
	// strength of the relationship itself. An explicit issue LINK is not: it
	// records that somebody drew a line between two tickets, which is applied
	// liberally and reciprocally, so it is scored like any other lead.
	add := func(key, hint string, structural bool) {
		key = strings.ToUpper(strings.TrimSpace(key))
		if key == "" || seen[key] || !issueKey.MatchString(key) {
			return
		}
		seen[key] = true
		refs = append(refs, source.Reference{
			ID: s.browseURL(key), Hint: hint, Structural: structural,
		})
	}

	if issue.Fields.Parent != nil {
		add(issue.Fields.Parent.Key, issue.Fields.Parent.Fields.Summary, true)
	}
	for _, sub := range issue.Fields.Subtasks {
		add(sub.Key, sub.Fields.Summary, true)
	}

	// Children, which the issue itself does not report.
	//
	// Jira's hierarchy runs Epic -> Task -> Sub-task, and only the LAST of
	// those edges appears in the `subtasks` field. An epic's children carry
	// `parent: THE-EPIC` on themselves, and nothing on the epic points back —
	// verified live against a real site, where an epic with three children
	// returned `subtasks: []`, `issuelinks: []` and no `parent`.
	//
	// So the only way down the hierarchy is to ask for it. Seeding a crawl
	// with an epic — the obvious thing to do, since an epic is the unit a
	// person thinks in — otherwise retrieved exactly one issue and stopped.
	for _, child := range s.children(ctx, issue.Key) {
		add(child.key, child.summary, true)
	}
	for _, link := range issue.Fields.IssueLinks {
		// The link's own type is part of the hint — "blocks" and "duplicates"
		// say something about the issue at the other end, and the hint is all
		// the frontier has to order by before retrieving it.
		if link.OutwardIssue != nil {
			add(link.OutwardIssue.Key, link.Type.Outward+" "+link.OutwardIssue.Fields.Summary, false)
		}
		if link.InwardIssue != nil {
			add(link.InwardIssue.Key, link.Type.Inward+" "+link.InwardIssue.Fields.Summary, false)
		}
	}
	return refs
}

// Bounds on the child search. A page is one request; the page cap exists only
// so that a pathological epic cannot turn one retrieval into hundreds of
// requests, and hitting it is logged rather than passed over.
const (
	childPageSize = 100
	maxChildPages = 10
)

type childRef struct{ key, summary string }

// children finds the issues whose parent is key.
//
// A search, not a field read, because Jira offers no field for it: the edge is
// recorded on the child. That costs one extra request per issue retrieved,
// which is the price of the hierarchy being navigable downwards at all.
//
// Failures are logged and swallowed. An issue whose children cannot be listed
// is still worth the document it produced, and a crawl that abandoned an issue
// because a search failed would lose the issue as well as its children.
func (s *Source) children(ctx context.Context, key string) []childRef {
	if key == "" {
		return nil
	}
	var (
		out   []childRef
		token string
	)
	for page := 0; page < maxChildPages; page++ {
		endpoint := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&fields=summary&maxResults=%d",
			s.apiBase(), url.QueryEscape("parent = "+key), childPageSize)
		if token != "" {
			endpoint += "&nextPageToken=" + url.QueryEscape(token)
		}

		var result struct {
			NextPageToken string `json:"nextPageToken"`
			IsLast        bool   `json:"isLast"`
			Issues        []struct {
				Key    string `json:"key"`
				Fields struct {
					Summary string `json:"summary"`
				} `json:"fields"`
			} `json:"issues"`
		}
		if err := s.get(ctx, endpoint, &result); err != nil {
			s.logger().Warn("jira: could not list an issue's children",
				"issue", key, "err", err)
			return out
		}
		for _, issue := range result.Issues {
			out = append(out, childRef{key: issue.Key, summary: issue.Fields.Summary})
		}
		token = result.NextPageToken
		if result.IsLast || token == "" || len(result.Issues) == 0 {
			return out
		}
	}
	s.logger().Warn("jira: stopped listing children at the page cap; some were not followed",
		"issue", key, "listed", len(out), "page_cap", maxChildPages)
	return out
}

// logger is never nil, so a Source built without one still works: this
// package is used by the orchestrator, by tests, and by diagnostics, and only
// the first of those has a logger to give.
func (s *Source) logger() *logging.Logger {
	if s.cfg.Logger != nil {
		return s.cfg.Logger
	}
	return logging.NewWithLevel(logging.LevelError, io.Discard)
}

// resolveAuth works out which scheme this token wants, once.
//
// Atlassian issues two kinds of API token and gives no way to tell them apart
// by looking: both begin "ATATT", both are about 192 characters, both end in
// the same checksum shape. A CLASSIC token authenticates as HTTP Basic against
// the site; a SCOPED one carries OAuth scopes and is only accepted as a bearer
// credential against api.atlassian.com. Present a scoped token the classic way
// and the answer is 401 — the same answer a revoked token gets, which is how a
// perfectly good token came to look expired.
//
// So it is probed rather than configured. One extra request on the first call
// of a run, and the alternative is asking a user to know which button they
// pressed on a page that does not label them clearly.
func (s *Source) resolveAuth(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != "" {
		return s.mode, nil
	}
	status, err := s.probe(ctx, authBasic, s.cfg.BaseURL)
	// A transport failure is not a credential failure and must not be reported
	// as one: a refused private address, a DNS failure or an unreachable site
	// all arrive here, and telling somebody their token was rejected sends
	// them to rotate a credential that is fine.
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		s.mode = authBasic
		return s.mode, nil
	}
	if cloud, cloudErr := s.resolveCloudID(ctx); cloudErr == nil && cloud != "" {
		status, err = s.probe(ctx, authBearer, s.cloudBase(cloud))
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			s.mode, s.cloudID = authBearer, cloud
			return s.mode, nil
		}
	}
	return "", fmt.Errorf("%w: %s rejected the API token for %s, as a classic token and as a "+
		"scoped one — check it has not been revoked, and that the email matches the account",
		source.ErrForbidden, s.cfg.BaseURL, s.cfg.Email)
}

// probe asks /myself, the one endpoint that answers 401 rather than 404 when
// it does not recognise the caller.
func (s *Source) probe(ctx context.Context, mode, base string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/api/3/myself", nil)
	if err != nil {
		return 0, err
	}
	s.authorize(req, mode)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		if errors.Is(err, netguard.ErrPrivateAddress) {
			return 0, fmt.Errorf("%w: %v", source.ErrOutOfScope, err)
		}
		return 0, fmt.Errorf("jira: cannot reach %s: %w", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// resolveCloudID names this site on api.atlassian.com. The endpoint is
// unauthenticated, which is what lets it run before the token is understood.
func (s *Source) resolveCloudID(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/_edge/tenant_info", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		CloudID string `json:"cloudId"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	return info.CloudID, json.Unmarshal(body, &info)
}

func (s *Source) cloudBase(cloudID string) string {
	host := s.apiHost
	if host == "" {
		host = "https://api.atlassian.com"
	}
	return host + "/ex/jira/" + cloudID
}

// authorize applies the scheme a mode implies.
func (s *Source) authorize(req *http.Request, mode string) {
	if mode == authBearer {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
		return
	}
	req.Header.Set("Authorization", s.auth)
}

// apiBase is the host to address, which differs by scheme.
func (s *Source) apiBase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == authBearer && s.cloudID != "" {
		return s.cloudBase(s.cloudID)
	}
	return s.cfg.BaseURL
}

// get performs one authenticated API call.
func (s *Source) get(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	mode := s.mode
	s.mu.Unlock()
	if mode == "" {
		mode = authBasic
	}
	s.authorize(req, mode)
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		// A refused private address is a scope error, not a transport
		// failure: the site was never ours to reach.
		if errors.Is(err, netguard.ErrPrivateAddress) {
			return fmt.Errorf("%w: %v", source.ErrOutOfScope, err)
		}
		return fmt.Errorf("jira: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Atlassian returns 404 both for "no such issue" and for "you may not
		// see this issue", deliberately, so that a stranger cannot enumerate
		// keys. Reported as not-found because that is all we can honestly say.
		return fmt.Errorf("%w: the issue does not exist, or the token cannot see it", source.ErrNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: the API token was rejected (%d)", source.ErrForbidden, resp.StatusCode)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: rate limited by the site", source.ErrExhausted)
	default:
		return fmt.Errorf("jira: unexpected status %d", resp.StatusCode)
	}

	// Bounded: an issue with a long comment thread is large, and a crawl reads
	// thousands of them.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("jira: read response: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("jira: decode response: %w", err)
	}
	return nil
}

var _ source.Source = (*Source)(nil)

// Verify checks the credentials before a run spends anything on them, and
// settles which of Atlassian's two authentication schemes this token wants.
//
// /myself is the endpoint to ask, because it answers 401 when the credentials
// are wrong. The issue endpoints answer 404 for both "no such issue" and "not
// authenticated" — Atlassian conflates them on purpose, so a stranger cannot
// discover which keys exist — which makes them useless for telling a person
// what went wrong.
func (s *Source) Verify(ctx context.Context) error {
	_, err := s.resolveAuth(ctx)
	return err
}

var _ source.Verifier = (*Source)(nil)
