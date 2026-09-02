package crawler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enact/internal/jira"
	"enact/internal/logging"
	"enact/internal/source"
	"enact/internal/wsd"
)

// chainSite serves SCRUM-1..SCRUM-6, each linked to the next, so a traversal
// that does not stop walks the whole chain. Every issue is about the same
// subject, so nothing but the depth limit can halt it.
func chainSite(t *testing.T, n int) (*httptest.Server, *int) {
	t.Helper()
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authentication is resolved before anything is retrieved, so a site
		// that cannot answer /myself retrieves nothing at all.
		if strings.HasSuffix(r.URL.Path, "/myself") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountId":"abc"}`))
			return
		}
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var index int
		if _, err := fmt.Sscanf(key, "SCRUM-%d", &index); err != nil || index < 1 || index > n {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		served++
		next := ""
		if index < n {
			next = fmt.Sprintf(`,"issuelinks":[{"type":{"inward":"is blocked by","outward":"blocks"},
				"outwardIssue":{"key":"SCRUM-%d","fields":{"summary":"indexing throughput work"}}}]`, index+1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"key":"%s","fields":{
			"summary":"indexing throughput work %d",
			"description":{"type":"doc","content":[{"type":"paragraph","content":[
			  {"type":"text","text":"The indexer stalls under load and the queue grows without bound."}]}]},
			"issuetype":{"name":"Task"},"status":{"name":"Open"}%s}}`, key, index, next)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

func jiraRelevance(t *testing.T) *Relevance {
	t.Helper()
	tax := testTaxonomy(t)
	inv := wsd.NewWordNetInventory(tax)
	rel, err := PrepareRelevance(context.Background(), tax, inv, inv,
		"indexer stalls under load with a growing queue",
		RelevanceConfig{Alpha: wsd.DefaultAlpha, SalientTerms: 30})
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// TestJIRADepthLimitStopsTheTraversal is the assurance that matters: an issue
// graph is dense and reciprocal, so without a limit a crawl reads the backlog.
//
// It runs the REAL crawl loop against a real (fake) Atlassian, which also
// proves the abstraction: the loop has no idea it is not crawling the web.
func TestJIRADepthLimitStopsTheTraversal(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		srv, served := chainSite(t, 6)
		src, err := jira.New(jira.Config{
			BaseURL: srv.URL, Email: "a@b.c", Token: "t", MaxDepth: depth,
			AllowPrivateAddresses: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		c := New(NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0}), logging.New())
		result, err := c.Run(context.Background(), jiraRelevance(t), src, Options{
			Seeds: []string{"SCRUM-1"},
			// Deliberately generous, so the JIRA depth limit is the only thing
			// that can stop this and the test cannot pass for the wrong reason.
			MaxPages: 50, MaxDepth: 20, ScoreThreshold: 0,
		}, nil)
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}

		// Seed plus one issue per hop followed.
		want := depth + 1
		if *served != want {
			t.Errorf("depth %d: retrieved %d issues, want %d — the traversal did not stop where it should",
				depth, *served, want)
		}
		if result.Source != "jira" {
			t.Errorf("result.Source = %q, want jira", result.Source)
		}
		// And it stopped because it ran out of frontier, not out of budget.
		if result.StopReason == ReasonPageBudget {
			t.Errorf("depth %d: hit the page budget; the depth limit is not doing the work", depth)
		}
	}
}

// TestJIRACrawlStoresIssues is the abstraction working end to end: an issue
// tracker filling a knowledge base through the same loop, scoring and report as
// a website.
func TestJIRACrawlStoresIssues(t *testing.T) {
	srv, _ := chainSite(t, 3)
	src, err := jira.New(jira.Config{BaseURL: srv.URL, Email: "a@b.c", Token: "t", AllowPrivateAddresses: true})
	if err != nil {
		t.Fatal(err)
	}
	c := New(NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0}), logging.New())
	result, err := c.Run(context.Background(), jiraRelevance(t), src, Options{
		Seeds: []string{"SCRUM-1"}, MaxPages: 10, MaxDepth: 5, ScoreThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stored) == 0 {
		t.Fatal("no issues stored")
	}
	first := result.Stored[0]
	if !strings.HasPrefix(first.Title, "SCRUM-") {
		t.Errorf("title = %q, want the issue key first", first.Title)
	}
	// The reference ID is the browse URL: canonical, and what a reader clicks.
	if !strings.Contains(first.URL, "/browse/SCRUM-") {
		t.Errorf("stored URL = %q, want a browse URL", first.URL)
	}
	if !strings.Contains(first.Text, "indexer stalls under load") {
		t.Errorf("the description did not survive ADF flattening: %q", first.Text)
	}
}

// TestJIRAOutOfProjectIsRecordedNotFetched: scope is the source's judgement,
// and a refusal must still appear in the report.
func TestJIRAOutOfProjectIsRecordedNotFetched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/myself") {
			_, _ = w.Write([]byte(`{"accountId":"abc"}`))
			return
		}
		fmt.Fprint(w, `{"key":"SCRUM-1","fields":{"summary":"indexing work",
		  "description":{"type":"doc","content":[{"type":"paragraph","content":[
		    {"type":"text","text":"The indexer stalls under load repeatedly."}]}]},
		  "issuelinks":[{"type":{"outward":"relates to"},
		    "outwardIssue":{"key":"OTHER-9","fields":{"summary":"unrelated"}}}]}}`)
	}))
	t.Cleanup(srv.Close)

	src, err := jira.New(jira.Config{
		BaseURL: srv.URL, Email: "a@b.c", Token: "t", Projects: []string{"SCRUM"},
		AllowPrivateAddresses: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0}), logging.New())
	result, err := c.Run(context.Background(), jiraRelevance(t), src, Options{
		Seeds: []string{"SCRUM-1"}, MaxPages: 10, MaxDepth: 5, ScoreThreshold: 0,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range result.Graph.Nodes {
		if strings.Contains(n.URL, "OTHER-9") {
			found = true
			if n.Status != StatusStopped || n.Reason != ReasonOffDomain {
				t.Errorf("out-of-project issue recorded as %s/%s, want stopped/off-domain",
					n.Status, n.Reason)
			}
		}
	}
	if !found {
		t.Error("the out-of-project issue is absent from the graph; a refusal must still be reported")
	}
}

// stubSource is a fixed graph, so the loop's treatment of structural edges can
// be tested without an HTTP fixture in the way.
type stubSource struct {
	docs      map[string]source.Document
	retrieved []string
}

func (s *stubSource) Name() string { return "stub" }
func (s *stubSource) Parse(seed string) (source.Reference, error) {
	return source.Reference{ID: seed}, nil
}
func (s *stubSource) Allows(source.Reference) bool { return true }
func (s *stubSource) Close() error                 { return nil }
func (s *stubSource) Retrieve(_ context.Context, ref source.Reference) (source.Document, error) {
	s.retrieved = append(s.retrieved, ref.ID)
	doc, ok := s.docs[ref.ID]
	if !ok {
		return source.Document{}, source.ErrNotFound
	}
	return doc, nil
}

// TestStructuralReferencesAreFollowedBelowTheThreshold.
//
// The priority of a link is a guess from text — 0.6*parent + 0.4*anchor
// overlap — which is meaningful for a web link and meaningless for an epic's
// child. Measured on a real crawl: an epic scoring 0.383 gave every child
// 0.230 against a 0.25 threshold, so two of the three issues in one piece of
// work were unreachable because their summaries did not repeat a query word.
//
// Rearranged, the old rule said a hint-less child could only be followed if
// its PARENT scored above threshold/0.6 — a fact about the parent that says
// nothing about the child.
func TestStructuralReferencesAreFollowedBelowTheThreshold(t *testing.T) {
	const body = "The indexer stalls under load and the queue grows without bound, " +
		"so the indexer falls behind and the load on the queue keeps growing."
	src := &stubSource{docs: map[string]source.Document{
		"epic": {Title: "epic", Text: body, References: []source.Reference{
			// No anchor overlap at all, so both arrive at 0.6*parent.
			{ID: "child", Hint: "zzz qqq", Structural: true},
			{ID: "stranger", Hint: "zzz qqq"},
		}},
		"child":    {Title: "child", Text: body},
		"stranger": {Title: "stranger", Text: body},
	}}

	c := New(NewFetcher(FetchConfig{AllowPrivateAddresses: true, HostDelay: 0}), logging.New())
	// A threshold above anything a hint-less child can inherit, which is the
	// situation the real crawl was in.
	result, err := c.Run(context.Background(), jiraRelevance(t), src, Options{
		Seeds: []string{"epic"}, MaxDepth: 3, MaxPages: 10, ScoreThreshold: 0.9,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, url := range src.retrieved {
		got[url] = true
	}
	if !got["child"] {
		t.Errorf("the structural reference was not retrieved; got %v", src.retrieved)
	}
	if got["stranger"] {
		t.Errorf("a non-structural reference below the threshold was retrieved: %v", src.retrieved)
	}

	// The report says which edges were guaranteed a place rather than earning
	// one — the priority alone no longer distinguishes them.
	for _, e := range result.Graph.Edges {
		if e.To == "child" && !e.Structural {
			t.Error("the structural edge was not marked in the report")
		}
		if e.To == "stranger" && e.Structural {
			t.Error("an ordinary edge was marked structural")
		}
	}
}
