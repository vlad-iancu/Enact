// Package source is the seam between "how a crawl explores" and "what it
// explores".
//
// A focused crawl is a search: take the most promising unexplored thing, look
// at it, score it, and let what it points at become the next candidates. None
// of that is about HTTP. It is about there being *references* you can hold
// without having looked at them, and a way to turn one into text plus more
// references.
//
// The web is one instance of that shape — a reference is a URL, retrieving is
// a GET, and the new references are the links on the page. A JIRA project is
// another — a reference is an issue key, retrieving is an API call, and the new
// references are its subtasks and linked issues. The crawl loop should not be
// able to tell them apart, and after this package it cannot.
package source

import (
	"context"
	"errors"
)

// Reference is something that can be retrieved but has not been yet.
//
// The crawl holds thousands of these — a frontier is a heap of them — so it is
// deliberately small and deliberately free of anything that requires having
// looked at the thing already.
type Reference struct {
	// ID identifies the reference and is its identity everywhere: the frontier
	// deduplicates on it, the graph is keyed by it, the knowledge base names
	// documents after it, and a repeat run recognises an unchanged page by it.
	// It must be stable across runs and canonical — two spellings of one thing
	// must produce one ID, or a crawl will fetch it twice and store it twice.
	//
	// A URL for the web; for an issue tracker, the browse URL of the issue,
	// which is both canonical and the thing a reader wants to click.
	ID string `json:"url"`
	// Hint is what is known about the reference before retrieving it: the
	// anchor text of a link, the summary of a linked issue. It is what makes
	// the frontier better than breadth-first, since it is the only evidence
	// available at the moment the ordering decision is made.
	Hint string `json:"anchor,omitempty"`
	// Depth is how many retrievals away from a seed this is.
	Depth int `json:"depth"`
	// Score is the priority the frontier orders by.
	Score float64 `json:"score"`
	// Structural marks a reference that belongs to the same piece of work as
	// the document it came from — an epic's child, a subtask, a parent. Such a
	// reference is always retrieved within the depth limit, and relevance
	// decides only whether the result is worth STORING.
	//
	// It exists because the default priority is a guess made from text —
	// `0.6 * parent score + 0.4 * overlap between the query and the anchor` —
	// and that guess is meaningful for a web link and meaningless for an
	// epic's child. Measured on a real crawl: an epic scoring 0.383 gave its
	// children 0.230 against a 0.25 threshold, so two of the three issues in a
	// piece of work were unreachable because their one-line summaries did not
	// repeat a query word. Rearranged, the old rule said no hint-less child
	// could ever be followed unless its parent scored above
	// `threshold / 0.6` — a bar about the PARENT that says nothing about the
	// child.
	//
	// Only for edges that are facts about the graph. An issue merely LINKED to
	// another ("relates to") is not structural: those are applied liberally and
	// reciprocally, and following them unconditionally reads a whole backlog.
	Structural bool `json:"structural,omitempty"`
}

// Document is what a retrieval produced.
type Document struct {
	// Title labels the document in the report and names it in the knowledge
	// base. It is NOT scored, so a source whose title carries meaning must put
	// it in Text as well — a title is often the only place a page or an issue
	// says what it is about, and leaving it here alone loses it.
	Title string
	// Text is the readable content: what is hashed for change detection and
	// what is stored in the knowledge base.
	Text string
	// Scored is the part of Text that relevance should judge, when the two
	// differ. Empty — the usual case — means Text itself.
	//
	// They differ when a source RENDERS a record rather than finding prose.
	// An issue tracker returns fields, and turning those into a document means
	// writing labels: "Summary:", "Status:", "Priority:". Useful to a person
	// reading the stored document, and poison to a relevance function.
	// Measured on a real ticket: ten of its twelve tokens were scaffolding,
	// every ticket carried the same ten so they all looked alike, and — worst
	// — the labels are Title Case, so the entity recogniser read "Summary",
	// "Progress" and "Priority" as NAMES and weighted them triple, even
	// welding "In Progress" and "Priority" across a line break into one
	// invented multi-word name.
	//
	// So a source that renders may say which part a person actually wrote.
	Scored string
	// References are what this document points at. Returned even when the
	// document itself is irrelevant: a page can be worthless and still be the
	// only route to what matters, and deciding otherwise is the crawl's job,
	// not the source's.
	References []Reference
	// Selected reports that a configured rule chose the text, rather than the
	// source inferring it. Reported per document because "the rule did not
	// match" and "the rule matched and found nothing" look identical from a
	// score and are fixed by opposite edits.
	Selected bool
}

// ForScoring is the text relevance should judge: Scored when a source supplied
// it, Text otherwise. One accessor so no caller has to remember the fallback,
// and so scoring the wrong field is a compile error rather than a quiet
// regression.
func (d Document) ForScoring() string {
	if d.Scored != "" {
		return d.Scored
	}
	return d.Text
}

// Source is a searchable space: somewhere references come from, and a way to
// turn one into content plus more references.
type Source interface {
	// Name identifies the implementation, for reports and logs.
	Name() string

	// Parse turns a seed as a user wrote it into a reference. This is where a
	// source decides what a user is even allowed to type — a URL, an issue
	// key, a project — and where a malformed one is rejected with a message
	// that names the shape expected.
	Parse(seed string) (Reference, error)

	// Allows reports whether a reference is in scope. Called before retrieval,
	// so a source can decline without spending anything: the web uses it to
	// stay on the crawl's domains, an issue tracker to stay within a project.
	Allows(ref Reference) bool

	// Retrieve fetches one reference. Errors should wrap the sentinels below
	// so the crawl can record WHY without knowing what kind of source it is
	// talking to.
	Retrieve(ctx context.Context, ref Reference) (Document, error)

	// Close releases anything the source holds open.
	Close() error
}

// Verifier is an optional interface: a source that can check its credentials
// before a run spends anything should.
//
// It exists because of a real diagnosis that went the wrong way. Atlassian
// answers 404 — not 401 — on its issue endpoints when a request is
// unauthenticated, deliberately, so that a stranger cannot enumerate issue
// keys. So an expired token produces "the issue does not exist, or the token
// cannot see it" for every issue, and the person reading that goes looking for
// the issue. Verified against a live site: the identical 404 arrived with a
// dead token and with no credentials at all.
//
// One call to an endpoint that DOES distinguish turns that into "the API token
// was rejected", which is the sentence somebody can act on.
type Verifier interface {
	Verify(ctx context.Context) error
}

// Retrieval failures the crawl records distinctly. Anything else is reported
// as an error against that one reference and the crawl continues — one bad
// page has never been a reason to abandon a search.
var (
	// ErrNotRetrievable: the reference exists but holds nothing readable — a
	// PDF, an image, an issue with an empty description.
	ErrNotRetrievable = errors.New("source: nothing readable at this reference")
	// ErrForbidden: the source refused. robots.txt, a 403, an issue the
	// credentials cannot see.
	ErrForbidden = errors.New("source: refused")
	// ErrNotFound: the reference does not exist.
	ErrNotFound = errors.New("source: not found")
	// ErrOutOfScope: Allows would have said no. Retrieve should also enforce
	// it, because a redirect can land somewhere Allows was never asked about.
	ErrOutOfScope = errors.New("source: out of scope")
	// ErrExhausted: a quota or allowance is spent. Unlike the others this
	// stops the run, because the next reference would fail the same way.
	ErrExhausted = errors.New("source: allowance exhausted")
)
