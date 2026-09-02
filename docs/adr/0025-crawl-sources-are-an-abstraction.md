# ADR-0025: A crawl explores a source; the web is one implementation

**Date**: 2026-09-01
**Status**: accepted (generalises [ADR-0022](0022-focused-crawling-with-knowledge-based-relevance.md))
**Deciders**: platform owner, Claude

## Context

ADR-0022 built a focused crawler for the web, and the web was baked in
everywhere: the frontier held URLs, scope was a domain comparison the loop
made itself, and the loop's own `Options` carried a domain allow-list, CSS
selectors and HTTP credential headers.

But almost none of what the crawler does is about HTTP. It is a search: take
the most promising unexplored thing, look at it, score it, and let what it
points at become the next candidates. That shape fits anything you can hold a
handle to without having looked at it yet.

The concrete need was an issue tracker. A JIRA ticket is retrieved by API call,
not GET; it has no `<main>`; and what it points at is subtasks and linked
issues, not anchors. Adding that as a special case inside the loop would have
meant `if source == "jira"` in the frontier, the scope check, the extractor and
the error classifier.

## Decision

The retrieval layer is an interface. `internal/source`:

```go
type Reference struct { ID, Hint string; Depth int; Score float64 }
type Document  struct { Title, Text string; References []Reference }

type Source interface {
    Name() string
    Parse(seed string) (Reference, error)      // what a user may type
    Allows(ref Reference) bool                 // what "in scope" means
    Retrieve(ctx, Reference) (Document, error)
    Close() error
}
```

`crawler.WebSource` and `jira.Source` implement it. A crawl names which one it
explores (`source: "web" | "jira"`, defaulting to web).

Everything web-specific moved off the loop's `Options` and onto `WebConfig`.
That is the test of whether this is an abstraction or a wrapper: **`Options` no
longer mentions the web.** What remains are bounds on the search.

Retrieval failures are reported through source-neutral sentinels
(`ErrNotFound`, `ErrForbidden`, `ErrNotRetrievable`, `ErrOutOfScope`,
`ErrExhausted`) so the loop records *why* without knowing what it is talking to
— "robots.txt refused" and "the token cannot see this issue" arrive at the same
place.

For JIRA, the traversal follows the edges meaning *part of the same piece of
work* — parent, children, subtasks, explicit issue links — and not mentions in
text.

A reference may be marked **structural**: it belongs to the same piece of work
as the document that produced it. A structural reference is retrieved within
the depth limit regardless of its priority, and relevance decides only whether
the result is worth **storing**. Which edges qualify is the source's judgement,
because only the source knows what its edges mean — JIRA marks parent, child
and subtask, and deliberately does not mark "relates to" or "blocks", which are
applied liberally and reciprocally. The web source marks nothing, so web crawls
are unaffected.

This exists because a link's priority is `0.6 * parent score + 0.4 * overlap
between the query and the anchor` — a guess from text, which is meaningful for
a web link and meaningless for an epic's child. Measured live: an epic scoring
0.383 gave every child 0.230 against a 0.25 threshold, so two of the three
issues in one piece of work were unreachable because their one-line summaries
did not repeat a query word. Rearranged, the old rule said a hint-less child
could only be followed if its **parent** scored above `threshold / 0.6` — a
fact about the parent that says nothing about the child.

## Alternatives considered

### Alternative 1: A second crawler for issue trackers
- **Pros**: no refactor; nothing existing can break.
- **Cons**: the frontier, the scoring, the knowledge-base sync, the report and
  the incremental re-crawl are none of them web-specific, and all would be
  duplicated.
- **Why not**: the duplication is nearly the whole crawler.

### Alternative 2: A `type` switch inside the crawl loop
- **Pros**: smallest change.
- **Cons**: the branch appears in four places — parse, scope, retrieve,
  classify — and every future source adds four more.
- **Why not**: the seam exists whether or not it is named; better to name it.

### Alternative 3: Keep `Options` as it was and pass a source alongside
- **Pros**: no signature churn for callers.
- **Cons**: a JIRA crawl would still carry `AllowedDomains` and
  `ExtractionRules` that mean nothing to it.
- **Why not**: leaving web concerns in a source-neutral struct is how the
  abstraction rots.

## Consequences

### Positive
- A new source is one file implementing four methods. Nothing in the loop, the
  frontier, the scoring or the report changes.
- Scope became the source's judgement rather than a domain comparison —
  staying on a site and staying inside a project are the same decision.
- `Candidate` is an alias for `source.Reference`, so the persisted frontier of
  a paused run still deserialises and the report's wire format is unchanged.

### Negative
- **A structural edge is exempt from the focus heuristic, so a JIRA crawl is
  bounded by depth and `max_pages` rather than by relevance.** An epic with
  four hundred children reads four hundred issues at `jira.max_depth` 1. That
  is the deliberate trade: the parts of one piece of work were grouped by a
  person, and second-guessing that from a one-line summary was what failed.
  Priorities are floored at the threshold rather than ignored, so best-first
  ordering still holds — a structural reference that also looks relevant is
  visited sooner, the rest queue at the bar behind everything the query
  favours — and relevance still decides what is stored.
- A wide refactor of a working crawler, touching the loop, the frontier, the
  runner and every crawl test.
- `Reference.ID` is a string doing double duty as identity and as something a
  reader clicks. It works because a JIRA issue has a browse URL, and would need
  revisiting for a source whose references are not addressable.

### Risks
- **The refactor introduced a scope escape, caught by an existing test.**
  `WebSource.Allows` reimplemented the check via `RegistrableDomain`, which
  takes a *raw URL* and parses it — passing a bare hostname returned `""` for
  both sides and allowed every off-site link. It now delegates to `SameSite`.
  One definition of "on-site", not two.
- **A new source silently loses the guarantees the old one carried.** The JIRA
  source was written with a plain `http.Client` and so had no SSRF protection:
  a `base_url` of `http://127.0.0.1` was accepted, fetched, and its response
  stored in a knowledge base. Verified live, then fixed by moving the dialer
  guard into `internal/netguard` and using it from both. ADR-0022 documented
  this risk and mitigated it *inside the crawler*, which is precisely why the
  second implementation did not inherit it. An interface makes adding a source
  easy; it does not make the new one safe, and the checklist for a source is
  now: does it dial a user-supplied address, and does it use netguard.

- **Atlassian issues two kinds of API token and gives no way to tell them
  apart.** Both begin `ATATT`, both are ~192 characters, both end in the same
  checksum shape; a CLASSIC one authenticates as Basic against the site, a
  SCOPED one only as a bearer credential against `api.atlassian.com`. Presented
  the wrong way, a perfectly good token gets 401 — the same answer a revoked
  one gets. Diagnosed live from a user's crawl, which reported every issue as
  missing. The scheme is now probed once per run rather than configured,
  because asking somebody which button they pressed on a page that does not
  label them clearly is not a usable question.

- **A source can only traverse the edges its API exposes, and Jira does not
  expose the one that matters most.** The hierarchy is Epic → Task →
  Sub-task, but only the last of those edges appears in a field: `subtasks`.
  An epic's children carry `parent` on themselves and nothing on the epic
  points back. Verified live — an epic with three children returned
  `subtasks: []`, `issuelinks: []` and no `parent`, and a crawl seeded with
  that epic retrieved exactly one issue and stopped. Seeding an epic is the
  natural thing to do, because an epic is the unit people plan in, so this was
  the common case failing silently rather than an edge case.

  The fix is a JQL search (`parent = KEY`) per issue retrieved, which costs one
  extra request per issue. That is the general lesson: `Retrieve` returning
  "the references this document points at" reads as a *field read*, and for
  the web it is one. For an API it may be a second query, and a source that
  only reads fields will traverse whatever subset the vendor happened to
  denormalise.

- **A source that RENDERS a record poisons its own relevance.** The web finds
  prose; an issue tracker returns fields, and turning fields into a document
  means writing labels — "Summary:", "Status:", "Priority:". Measured on a real
  ticket, ten of its twelve tokens were those labels and their
  fixed-vocabulary values; every issue on a site carries the same ten, so every
  issue resembled every other, which is backwards for a function whose job is
  to tell them apart. Worse, the labels are Title Case, so the entity
  recogniser read "Summary", "Progress" and "Priority" as NAMES — weighted 3×
  by [ADR-0024](0024-names-are-recognised-from-evidence.md) — and fused "In
  Progress" and "Priority" across a line break into an invented multi-word
  name.

  `Document.Scored` now separates what is judged from what is stored: the
  stored document keeps its structure, because that is what makes it useful
  coming back out of a knowledge base, and scoring sees only what a person
  wrote. Removing the scaffolding moved an irrelevant ticket from 0.243 to
  0.120 and a relevant one from 0.383 to 0.484 — the gap between them widened
  from 0.14 to 0.36.

- **JIRA bypasses the fetcher**, so it has no per-host delay and no
  concurrency cap. That is deliberate: an API a customer is entitled to call
  with their own token does not need the politeness a stranger's website does,
  and Atlassian answers 429 — mapped to `ErrExhausted`, which pauses the run —
  if it disagrees. The politeness machinery remains a property of the web
  implementation, and would need lifting only if a source appears that is both
  unmetered and somebody else's.

  What bounds a JIRA crawl instead is **depth**, and it is bounded twice: by
  the crawl's own `max_depth`, and by `jira.max_depth` (default 2, ceiling 4)
  which limits how far a RELATIONSHIP is followed. The second is the operative
  one, because the two mean different things — one bounds the search, the other
  bounds how far an edge is worth trusting — and an issue graph reaches the
  second first. A web link is one step to one page; an issue relationship is
  one step to every subtask, every linked issue and the parent, reciprocally.
