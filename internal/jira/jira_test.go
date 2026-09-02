package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"enact/internal/source"
)

// fakeSite serves the two issues a traversal test needs, and records what
// credentials it was shown.
func fakeSite(t *testing.T, issues map[string]string) (*httptest.Server, *string) {
	t.Helper()
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		// A working site answers /myself; a rejected token is what makes it
		// 401, which is the whole distinction Verify relies on.
		if strings.HasSuffix(r.URL.Path, "/myself") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountId":"abc","emailAddress":"someone@example.com"}`))
			return
		}
		// The JQL search, which is the only way to find an issue's children:
		// Jira records that edge on the CHILD, and nothing on the parent points
		// back. The fixture answers it the way the real API does — by looking
		// at every issue's own parent field — so a test cannot pass by the
		// fixture being more helpful than Atlassian.
		if strings.HasSuffix(r.URL.Path, "/search/jql") {
			jql := r.URL.Query().Get("jql")
			parent := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(jql, "parent =")))
			var found []string
			for _, body := range issues {
				var issue struct {
					Key    string `json:"key"`
					Fields struct {
						Summary string `json:"summary"`
						Parent  *struct {
							Key string `json:"key"`
						} `json:"parent"`
					} `json:"fields"`
				}
				if json.Unmarshal([]byte(body), &issue) != nil || issue.Fields.Parent == nil {
					continue
				}
				if strings.EqualFold(issue.Fields.Parent.Key, parent) {
					found = append(found, fmt.Sprintf(`{"key":%q,"fields":{"summary":%q}}`,
						issue.Key, issue.Fields.Summary))
				}
			}
			sort.Strings(found)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"isLast":true,"issues":[%s]}`, strings.Join(found, ","))
			return
		}
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := issues[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &sawAuth
}

func newTestSource(t *testing.T, base string, cfg Config) *Source {
	t.Helper()
	cfg.BaseURL = base
	cfg.AllowPrivateAddresses = true
	if cfg.Email == "" {
		cfg.Email = "someone@example.com"
	}
	if cfg.Token == "" {
		cfg.Token = "a-token"
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const epic = `{"key":"SCRUM-1","fields":{
  "summary":"Indexing stalls under load",
  "description":{"type":"doc","content":[
     {"type":"paragraph","content":[{"type":"text","text":"The indexer stops making progress."}]},
     {"type":"paragraph","content":[{"type":"text","text":"Restarting the consumer clears it."}]}]},
  "issuetype":{"name":"Epic"},"status":{"name":"In Progress"},"priority":{"name":"High"},
  "labels":["indexing","performance"],
  "subtasks":[{"key":"SCRUM-2","fields":{"summary":"Add a queue depth metric"}}],
  "issuelinks":[{"type":{"inward":"is blocked by","outward":"blocks"},
                 "outwardIssue":{"key":"SCRUM-3","fields":{"summary":"Upgrade the consumer"}}}],
  "comment":{"comments":[{"author":{"name":"Ana"},
     "body":{"type":"doc","content":[{"type":"paragraph","content":[
        {"type":"text","text":"Reproduced on staging."}]}]}}]}}}`

// TestParseAcceptsKeysAndBrowseURLs: a person typing has a key, a person
// pasting has a URL, and refusing either would be pedantry.
func TestParseAcceptsKeysAndBrowseURLs(t *testing.T) {
	s := newTestSource(t, "https://acme.atlassian.net", Config{})
	for _, seed := range []string{"SCRUM-1", "scrum-1", " SCRUM-1 ",
		"https://acme.atlassian.net/browse/SCRUM-1"} {
		ref, err := s.Parse(seed)
		if err != nil {
			t.Errorf("Parse(%q): %v", seed, err)
			continue
		}
		if ref.ID != "https://acme.atlassian.net/browse/SCRUM-1" {
			t.Errorf("Parse(%q) = %q, want the canonical browse URL", seed, ref.ID)
		}
	}
	for _, bad := range []string{"", "not an issue", "SCRUM", "-1", "https://acme.atlassian.net/"} {
		if _, err := s.Parse(bad); err == nil {
			t.Errorf("Parse(%q) was accepted", bad)
		}
	}
}

// TestAllowsStaysInsideItsProjects. A crawl that wandered from SCRUM into
// every project on the site would be a surprise, and an expensive one.
func TestAllowsStaysInsideItsProjects(t *testing.T) {
	s := newTestSource(t, "https://acme.atlassian.net", Config{Projects: []string{"SCRUM"}})
	in, _ := s.Parse("SCRUM-9")
	if !s.Allows(in) {
		t.Error("an issue in the crawl's own project was refused")
	}
	out, _ := s.Parse("OTHER-9")
	if s.Allows(out) {
		t.Error("an issue in another project was allowed")
	}
	// No projects named: the crawl is confined by its seeds instead.
	open := newTestSource(t, "https://acme.atlassian.net", Config{})
	if !open.Allows(out) {
		t.Error("with no projects configured every issue should be in scope")
	}
}

// TestRetrieveRendersAnIssueAndItsRelationships is the source doing its job.
func TestRetrieveRendersAnIssueAndItsRelationships(t *testing.T) {
	srv, sawAuth := fakeSite(t, map[string]string{"SCRUM-1": epic})
	s := newTestSource(t, srv.URL, Config{Email: "ana@example.com", Token: "secret-token"})

	ref, _ := s.Parse("SCRUM-1")
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	// Basic auth is email:token, which is what Atlassian expects.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ana@example.com:secret-token"))
	if *sawAuth != want {
		t.Errorf("Authorization = %q, want basic email:token", *sawAuth)
	}

	if doc.Title != "SCRUM-1 Indexing stalls under load" {
		t.Errorf("title = %q", doc.Title)
	}
	// The description arrives as Atlassian Document Format — a JSON tree, not
	// a string — so this also covers flattening it.
	for _, want := range []string{
		"Indexing stalls under load", "The indexer stops making progress",
		"Restarting the consumer clears it", "Reproduced on staging",
		"Epic", "In Progress", "High", "indexing, performance",
	} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text is missing %q; got:\n%s", want, doc.Text)
		}
	}

	// Subtasks and explicit links become references; the hint carries the link
	// type, which is all the frontier has to order by before retrieving.
	got := map[string]string{}
	for _, r := range doc.References {
		got[r.ID] = r.Hint
	}
	if hint, ok := got[srv.URL+"/browse/SCRUM-2"]; !ok {
		t.Error("the subtask did not become a reference")
	} else if !strings.Contains(hint, "queue depth") {
		t.Errorf("subtask hint = %q, want its summary", hint)
	}
	if hint, ok := got[srv.URL+"/browse/SCRUM-3"]; !ok {
		t.Error("the linked issue did not become a reference")
	} else if !strings.Contains(hint, "blocks") {
		t.Errorf("link hint = %q, want the link type", hint)
	}
}

// TestRelationshipsStopAtMaxDepth. Issue graphs are dense — "relates to" is
// applied liberally and reciprocally — so each hop multiplies rather than adds.
func TestRelationshipsStopAtMaxDepth(t *testing.T) {
	srv, _ := fakeSite(t, map[string]string{"SCRUM-1": epic})
	s := newTestSource(t, srv.URL, Config{MaxDepth: 1})

	ref, _ := s.Parse("SCRUM-1")
	shallow, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(shallow.References) == 0 {
		t.Fatal("depth 0 should still follow relationships")
	}
	ref.Depth = 1
	deep, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(deep.References) != 0 {
		t.Errorf("at the depth limit the traversal returned %d references, want none",
			len(deep.References))
	}
}

// TestErrorsMapToTheSourceSentinels lets the crawl record WHY without knowing
// what kind of source it is talking to.
func TestErrorsMapToTheSourceSentinels(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, source.ErrNotFound},
		{http.StatusUnauthorized, source.ErrForbidden},
		{http.StatusForbidden, source.ErrForbidden},
		{http.StatusTooManyRequests, source.ErrExhausted},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Authentication must succeed, or resolveAuth fails first and this
			// tests nothing about how an ISSUE's status is mapped.
			if strings.HasSuffix(r.URL.Path, "/myself") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"accountId":"abc"}`))
				return
			}
			w.WriteHeader(tc.status)
		}))
		s := newTestSource(t, srv.URL, Config{})
		ref, _ := s.Parse("SCRUM-1")
		_, err := s.Retrieve(context.Background(), ref)
		if err == nil {
			t.Errorf("status %d produced no error", tc.status)
		} else if !strings.Contains(err.Error(), strings.TrimPrefix(tc.want.Error(), "source: ")) {
			t.Errorf("status %d -> %v, want %v", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

// TestNewRequiresASiteAndCredentials.
func TestNewRequiresASiteAndCredentials(t *testing.T) {
	for _, cfg := range []Config{
		{Email: "a@b.c", Token: "t"},
		{BaseURL: "not a url", Email: "a@b.c", Token: "t"},
		{BaseURL: "https://acme.atlassian.net", Token: "t"},
		{BaseURL: "https://acme.atlassian.net", Email: "a@b.c"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) was accepted", cfg)
		}
	}
}

// TestFlattenADFHandlesBothShapes: v3 returns a tree, older responses a string.
func TestFlattenADFHandlesBothShapes(t *testing.T) {
	if got := flattenADF(json.RawMessage(`"just a string"`)); got != "just a string" {
		t.Errorf("plain string -> %q", got)
	}
	tree := `{"type":"doc","content":[{"type":"paragraph","content":[
	   {"type":"text","text":"one"},{"type":"text","text":" two"}]},
	   {"type":"paragraph","content":[{"type":"text","text":"three"}]}]}`
	got := flattenADF(json.RawMessage(tree))
	if !strings.Contains(got, "one two") || !strings.Contains(got, "three") {
		t.Errorf("tree -> %q", got)
	}
	if strings.Contains(got, "one twothree") {
		t.Error("paragraphs ran together; block boundaries must break the line")
	}
	if flattenADF(nil) != "" {
		t.Error("nil should flatten to empty")
	}
}

// TestBaseURLCannotReachPrivateAddresses is the SSRF guard.
//
// A base URL is user-supplied and fetched from the platform's own network
// position — exactly like a crawl's seed, and needing exactly the same
// protection. It did not have it: this source was originally written with a
// plain http.Client, and a crawl pointed at http://127.0.0.1 read a local
// service and filed the response in a knowledge base. Verified live before the
// fix, and this is what stops it coming back.
func TestBaseURLCannotReachPrivateAddresses(t *testing.T) {
	srv, _ := fakeSite(t, map[string]string{"SCRUM-1": epic})

	// The guard is ON by default: the test helper opts out, so this builds the
	// config directly to get the shipped behaviour.
	guarded, err := New(Config{BaseURL: srv.URL, Email: "a@b.c", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := guarded.Parse("SCRUM-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Retrieve(context.Background(), ref); err == nil {
		t.Fatal("a loopback base URL was reachable; the SSRF guard is not applied")
	} else if !strings.Contains(err.Error(), "out of scope") {
		t.Errorf("err = %v, want it reported as out of scope", err)
	}

	// And the opt-out still works, or the whole suite would be untestable.
	allowed := newTestSource(t, srv.URL, Config{})
	if _, err := allowed.Retrieve(context.Background(), ref); err != nil {
		t.Errorf("AllowPrivateAddresses did not permit the test server: %v", err)
	}
}

// TestVerifyNamesTheRealProblem pins the diagnosis that went wrong.
//
// Atlassian answers 404 on issue endpoints when unauthenticated — the same as
// for a genuinely missing issue — so a dead token made every issue report
// itself missing and sent the reader looking for the issue. /myself answers
// 401, so one call at the start of a run can say what is actually wrong.
func TestVerifyNamesTheRealProblem(t *testing.T) {
	// A site that rejects everything, the way a revoked token is seen.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/myself") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Issue endpoints: 404, indistinguishable from a missing issue.
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	s := newTestSource(t, srv.URL, Config{Email: "someone@example.com"})

	// Retrieve now settles the scheme first, so it reports the credentials
	// rather than passing on Atlassian's misleading 404. That IS the fix; what
	// follows checks the message is worth reading.
	ref, _ := s.Parse("SCRUM-1")
	if _, err := s.Retrieve(context.Background(), ref); err == nil {
		t.Fatal("a site rejecting everything produced no error")
	}

	// What Verify says.
	err := s.Verify(context.Background())
	if err == nil {
		t.Fatal("Verify accepted credentials the site rejects")
	}
	for _, want := range []string{"rejected the API token", "revoked", "email"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}

	// And it must not cry wolf when the credentials are good.
	ok, _ := fakeSite(t, map[string]string{"SCRUM-1": epic})
	good := newTestSource(t, ok.URL, Config{})
	if err := good.Verify(context.Background()); err != nil {
		t.Errorf("Verify rejected working credentials: %v", err)
	}
}

// TestScopedTokensUseBearerAgainstTheApiHost covers Atlassian's second kind of
// token, which is indistinguishable from the first by looking at it.
//
// Both begin "ATATT", both are ~192 characters, both end in the same checksum
// shape. A CLASSIC token authenticates as Basic against the site; a SCOPED one
// carries OAuth scopes and is only accepted as a bearer credential against
// api.atlassian.com. Presented the classic way, a scoped token gets 401 —
// exactly what a revoked one gets, which is how a working token came to look
// expired. So the scheme is probed rather than configured.
func TestScopedTokensUseBearerAgainstTheApiHost(t *testing.T) {
	var sawBearer, sawBasicRejected bool

	// The api.atlassian.com stand-in: accepts Bearer only.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawBearer = true
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/myself") {
			_, _ = w.Write([]byte(`{"accountId":"abc"}`))
			return
		}
		_, _ = w.Write([]byte(epic))
	}))
	t.Cleanup(api.Close)

	// The site: rejects Basic the way a scoped token is rejected, and serves
	// the tenant_info the cloud id comes from.
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_edge/tenant_info") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"cloudId":"test-cloud-id"}`))
			return
		}
		sawBasicRejected = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(site.Close)

	s := newTestSource(t, site.URL, Config{})
	// Point the bearer path at the stand-in.
	s.apiHost = api.URL

	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !sawBasicRejected {
		t.Error("the classic scheme was never tried; it must be, since most tokens are classic")
	}
	if !sawBearer {
		t.Error("the scoped scheme was never tried")
	}
	if s.mode != authBearer {
		t.Errorf("mode = %q, want %q", s.mode, authBearer)
	}

	// And retrieval must then address the api host, not the site.
	ref, _ := s.Parse("SCRUM-1")
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !strings.Contains(doc.Text, "Indexing stalls") {
		t.Errorf("issue not retrieved over the scoped path: %q", doc.Text)
	}
	// The reference ID stays the browse URL on the SITE — that is what a
	// person clicks, and api.atlassian.com is an implementation detail.
	if !strings.HasPrefix(ref.ID, site.URL) {
		t.Errorf("reference ID = %q, want a browse URL on the site", ref.ID)
	}
}

// TestEpicYieldsItsChildren pins the traversal that was missing.
//
// Jira's hierarchy is Epic -> Task -> Sub-task, and only the last edge shows up
// in the `subtasks` field. An epic reports NOTHING about its children: verified
// against a real site, an epic with three children returned `subtasks: []`,
// `issuelinks: []` and no `parent`. A crawl seeded with that epic — the natural
// thing to seed, since an epic is the unit people plan in — retrieved one issue
// and stopped.
func TestEpicYieldsItsChildren(t *testing.T) {
	issues := map[string]string{
		"SCRUM-2": `{"key":"SCRUM-2","fields":{"summary":"Finish paper",
			"issuetype":{"name":"Epic"},"status":{"name":"In Progress"},
			"subtasks":[],"issuelinks":[]}}`,
		"SCRUM-1": `{"key":"SCRUM-1","fields":{"summary":"Create a JIRA MCP server",
			"issuetype":{"name":"Task"},"parent":{"key":"SCRUM-2","fields":{"summary":"Finish paper"}}}}`,
		"SCRUM-3": `{"key":"SCRUM-3","fields":{"summary":"Create the paper",
			"issuetype":{"name":"Task"},"parent":{"key":"SCRUM-2","fields":{"summary":"Finish paper"}}}}`,
		"SCRUM-4": `{"key":"SCRUM-4","fields":{"summary":"Add new features",
			"issuetype":{"name":"Task"},"parent":{"key":"SCRUM-2","fields":{"summary":"Finish paper"}}}}`,
		// A different epic's child, which must not be dragged in.
		"OTHER-9": `{"key":"OTHER-9","fields":{"summary":"Unrelated",
			"issuetype":{"name":"Task"},"parent":{"key":"SCRUM-7","fields":{"summary":"Elsewhere"}}}}`,
	}
	srv, _ := fakeSite(t, issues)
	s := newTestSource(t, srv.URL, Config{MaxDepth: 2})

	ref, err := s.Parse("SCRUM-2")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	got := map[string]bool{}
	for _, r := range doc.References {
		got[r.ID] = true
	}
	for _, want := range []string{"SCRUM-1", "SCRUM-3", "SCRUM-4"} {
		if !got[s.browseURL(want)] {
			t.Errorf("epic did not yield %s; got %v", want, doc.References)
		}
	}
	if got[s.browseURL("OTHER-9")] {
		t.Error("another epic's child was followed")
	}

	// The depth cut still applies: at the limit an epic yields nothing, or the
	// bound would be advisory.
	deep, err := s.Retrieve(context.Background(), source.Reference{ID: ref.ID, Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(deep.References) != 0 {
		t.Errorf("children followed past max_depth: %v", deep.References)
	}
}

// TestChildLookupFailureKeepsTheIssue: an issue whose children cannot be listed
// is still worth its own document. Losing the parent because a search failed
// would turn a partial answer into no answer.
func TestChildLookupFailureKeepsTheIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/myself"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountId":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/search/jql"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"SCRUM-2","fields":{"summary":"Finish paper",
				"issuetype":{"name":"Epic"},"status":{"name":"Open"}}}`))
		}
	}))
	defer srv.Close()

	s := newTestSource(t, srv.URL, Config{MaxDepth: 2})
	ref, _ := s.Parse("SCRUM-2")
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatalf("a failed child search must not fail the retrieval: %v", err)
	}
	if !strings.Contains(doc.Text, "Finish paper") {
		t.Errorf("the issue's own text was lost: %q", doc.Text)
	}
}

// TestStructuralEdgesAreMarked. Which edges are facts about the issue graph
// and which are somebody's opinion is the SOURCE's judgement, not the crawl
// loop's — the loop follows structural references unconditionally, so getting
// this wrong either strands an epic's children or reads a whole backlog.
func TestStructuralEdgesAreMarked(t *testing.T) {
	issues := map[string]string{
		"SCRUM-2": `{"key":"SCRUM-2","fields":{"summary":"Finish paper",
			"issuetype":{"name":"Epic"},"status":{"name":"Open"},
			"subtasks":[{"key":"SCRUM-8","fields":{"summary":"A subtask"}}],
			"parent":{"key":"SCRUM-5","fields":{"summary":"An initiative"}},
			"issuelinks":[{"type":{"inward":"is blocked by","outward":"blocks"},
			   "outwardIssue":{"key":"SCRUM-6","fields":{"summary":"Something linked"}}}]}}`,
		"SCRUM-3": `{"key":"SCRUM-3","fields":{"summary":"A child",
			"issuetype":{"name":"Task"},"parent":{"key":"SCRUM-2","fields":{"summary":"Finish paper"}}}}`,
	}
	srv, _ := fakeSite(t, issues)
	s := newTestSource(t, srv.URL, Config{MaxDepth: 2})
	ref, _ := s.Parse("SCRUM-2")
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"SCRUM-5": true,  // parent
		"SCRUM-8": true,  // subtask
		"SCRUM-3": true,  // epic child, found by search
		"SCRUM-6": false, // "blocks" — an opinion, not a fact about the work
	}
	got := map[string]bool{}
	for _, r := range doc.References {
		got[r.ID[strings.LastIndex(r.ID, "/")+1:]] = r.Structural
	}
	for key, structural := range want {
		if actual, ok := got[key]; !ok {
			t.Errorf("%s was not referenced at all", key)
		} else if actual != structural {
			t.Errorf("%s structural = %v, want %v", key, actual, structural)
		}
	}
}

// TestScoredTextExcludesTheFieldLabels.
//
// The labels this package writes are for a person reading the stored document.
// A relevance function must not see them: measured on a real ticket, ten of its
// twelve tokens were labels and their fixed-vocabulary values, every ticket
// carried the same ten so they all looked alike, and because the labels are
// Title Case the entity recogniser read "Summary", "Progress" and "Priority" as
// NAMES and weighted them triple.
func TestScoredTextExcludesTheFieldLabels(t *testing.T) {
	issues := map[string]string{
		"SCRUM-2": `{"key":"SCRUM-2","fields":{"summary":"Finish paper",
			"issuetype":{"name":"Epic"},"status":{"name":"In Progress"},
			"priority":{"name":"Medium"},"labels":["research"],
			"components":[{"name":"Writing"}],
			"description":{"type":"doc","content":[{"type":"paragraph","content":[
			   {"type":"text","text":"The draft needs a conclusion."}]}]},
			"comment":{"comments":[{"body":{"type":"doc","content":[{"type":"paragraph",
			   "content":[{"type":"text","text":"Reviewed on Tuesday."}]}]}}]}}}`,
	}
	srv, _ := fakeSite(t, issues)
	s := newTestSource(t, srv.URL, Config{})
	ref, _ := s.Parse("SCRUM-2")
	doc, err := s.Retrieve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	// The stored document keeps its structure — that is what makes it useful
	// when it comes back out of a knowledge base.
	for _, want := range []string{"Issue: SCRUM-2", "Summary: Finish paper",
		"Status: In Progress", "Priority: Medium"} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("the stored text lost %q:\n%s", want, doc.Text)
		}
	}

	scored := doc.ForScoring()
	if scored == doc.Text {
		t.Fatal("scored text is the stored text; the labels are still being scored")
	}
	// Everything a person wrote survives.
	for _, want := range []string{"Finish paper", "The draft needs a conclusion",
		"Reviewed on Tuesday", "research", "Writing"} {
		if !strings.Contains(scored, want) {
			t.Errorf("scored text lost the content %q:\n%s", want, scored)
		}
	}
	// The scaffolding does not — including the fixed-vocabulary values, which
	// every issue on a site shares and which can only make two look alike.
	for _, unwanted := range []string{"Summary:", "Status:", "Priority:", "Type:",
		"Labels:", "Components:", "In Progress", "Medium", "Epic"} {
		if strings.Contains(scored, unwanted) {
			t.Errorf("scored text still carries %q:\n%s", unwanted, scored)
		}
	}
}
