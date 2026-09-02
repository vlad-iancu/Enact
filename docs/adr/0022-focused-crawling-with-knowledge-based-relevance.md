# ADR-0022: Focused crawling with knowledge-based relevance

**Date**: 2026-08-30
**Status**: accepted, partly superseded

**Deciders**: platform owner, Claude

> Later decisions changed parts of this one. The feature and its shape stand;
> these do not:
>
> * the query is no longer disambiguated word by word — [ADR-0023](0023-query-senses-are-chosen-jointly.md);
> * "pure Go, no native dependencies" no longer holds, and names are now a
>   first-class notion — [ADR-0024](0024-names-are-recognised-from-evidence.md);
> * the crawler is no longer web-only — [ADR-0025](0025-crawl-sources-are-an-abstraction.md);
> * crawls may hold credentials — [ADR-0026](0026-crawl-credentials-are-scoped-to-a-host.md).

## Context

Retrieval knowledge bases are filled by hand. The next capability is filling
one automatically from the web — but a general crawler is useless for this: it
follows every link and would fill a knowledge base with a site's navigation,
legal pages and unrelated articles.

What is wanted is a crawler that walks *towards a topic*: one that scores each
page against a natural-language query and uses that score to decide where to
go next, not merely what to keep. That requires a relevance function good
enough to be trusted with the decision, and it must judge meaning rather than
word overlap — a page about otter habitat is relevant to "marine mammal
range" without sharing a single content word with it.

## Decision

A crawl is a stored definition — query, seed URLs, an empty retrieval
knowledge base, bounds, and a repeat interval — executed by a best-first
crawler whose priority function is knowledge-based.

Relevance is `alpha * semantic + (1 - alpha) * BM25`, where the semantic half
is the Mihalcea, Corley & Strapparava (2006) text-to-text measure over
Wu-Palmer concept similarity, computed on **synsets** produced by extended
Lesk (Banerjee & Pedersen 2002) and expanded across a semantic graph with
decaying weights.

Two services, matching the workflows split: `enact-crawls` authors and queues,
`enact-crawl-orchestrator` schedules and executes. Every run produces a report
— the disambiguated and expanded query, and a graph of every document reached
with its score, its links, and the frontier nodes where the search stopped.

## Alternatives Considered

### Alternative 1: Embedding similarity instead of a sense inventory
- **Pros**: one Bedrock call per page, no WordNet, no BabelNet, no quota, and
  the platform already embeds text for retrieval.
- **Cons**: a black box. When a crawl fetches the wrong pages there is nothing
  to look at — an embedding cannot say *why* it thought a page was relevant.
- **Why not**: the report is half the feature. "The query's word `conservation`
  was read in its physics sense" is a diagnosis somebody can act on; "the
  cosine was 0.71" is not. A knowledge-based function is auditable by
  construction.

### Alternative 2: A Python NLP service
- **Pros**: nltk, trafilatura and the `babelnet` package are the reference
  implementations of every algorithm here.
- **Cons**: the only Python in an all-Go platform, a third service, and a new
  deployment and S2S story for it.
- **Why not**: `go-trafilatura` is a port of the same algorithm and scores
  higher on the standard benchmark than the readability alternative (F1 0.960
  vs 0.934); `prose` tags parts of speech and `gostuff/nlp/wordnet` computes
  Wu-Palmer. Extended Lesk, Mihalcea and BM25 are a few hundred lines.

### Alternative 3: BabelNet for both the query and the pages
- **Pros**: the richest vocabulary everywhere, including on pages full of
  named entities and jargon.
- **Cons**: measured against the live API, one word costs about 25 requests —
  a sense lookup, two per candidate sense (relations need a *separate*
  `getOutgoingEdges` call), and one per neighbour consulted. A 40-term page is
  the entire 1000-request daily allowance.
- **Why not**: it does not fit, and the failure mode is a crawl that stalls
  after one page. **Chosen instead**: BabelNet for the QUERY, which is short,
  cached forever, and where the richer vocabulary actually steers something;
  the local WordNet for every page. They are comparable because a BabelNet
  sense derived from WordNet carries its original offset.

  Having WordNet loaded anyway turns out to pay a second time: when the
  BabelNet allowance is spent, the query is re-analysed against WordNet and
  the run continues in a degraded mode it declares in its report, rather than
  pausing until the allowance returns.

### Alternative 4: Filter after a breadth-first crawl
- **Pros**: much simpler; no priority queue, no stopping rule.
- **Cons**: a budget of 200 pages spent breadth-first never leaves the seed's
  immediate neighbourhood, and there is no principled point at which to stop.
- **Why not**: "focused" is the whole feature. A best-first frontier spends
  the budget on the topic rather than on the site's shape, and the score
  supplies the stopping rule: when the best unvisited link is not worth
  fetching, nothing anywhere is.

## Consequences

### Positive
- A knowledge base can track a corpus that changes, without anybody uploading
  anything.
- The report explains every decision: which sense of each query word was
  chosen, what the query expanded to, what each page scored and why (the
  semantic and lexical halves are recorded separately), and where the search
  stopped.
- Re-crawling is incremental. The crawl keeps a URL-to-document map, so an
  unchanged page costs nothing — no upload, no re-embedding.
- The relevance function is reusable: `internal/wsd` knows nothing about
  crawling.

### Negative
- Two more services, three more indices, and a 36 MB WordNet download
  (`make wordnet`) that the orchestrator will not start without.
- WordNet parsing costs a few hundred milliseconds and ~150 MB of resident
  memory in the orchestrator.
- Word sense disambiguation is imperfect on short queries. Measured on "sea
  otter habitat diet and conservation", extended Lesk chose the *physics*
  sense of `conservation` and the *slimming* sense of `diet` — five words is
  thin context. The report is the only place this is visible.

  BM25 does **not** compensate for it, which was the original hope. Measured
  on a real crawl for "opensearch indices, security, syntax and usage", where
  `security` resolved to the collateral sense and `usage` to habit, the site's
  terms-of-service page beat its OpenSearch tag page on the *lexical* half
  (0.21 against 0.07): BM25 runs over the EXPANDED query, so it inherits the
  same wrong senses. Both halves of the score are downstream of
  disambiguation, and lowering `alpha` cannot rescue a query that was
  misunderstood.
- A run that falls back to WordNet still stores pages, and the incremental
  re-crawl then treats them as unchanged — so a degraded run's mediocre
  results persist into later, undegraded runs until the pages themselves
  change. `analysis.degraded` in the report is how this is diagnosed.
- The crawl is English-only in practice: `prose`'s tagger is, even though
  BabelNet is not.

- **A link's priority is an estimate made before retrieval, from the parent
  page's score and the anchor or URL slug — and it used to be frozen at the
  first sighting.** A page is linked from many places at very different
  priorities, and the first sighting is systematically the weakest: hubs and
  tag pages are reached early, score low on prose, and link to everything.
  Measured on a real dev.to run, **all 62 multiply-linked URLs had sightings
  that disagreed**, and two OpenSearch articles — squarely on topic — were
  seen at 0.129 and at 0.58. The frontier now raises a queued candidate when a
  better route finds it, carrying the new depth and hint with the score.
  Retrieved pages are still never re-queued, which is what stops a cycle.

- **The semantic half is unreliable on very short documents, and requiring an
  exact SYNSET match made it worse.** On a two-word JIRA ticket, the query's
  `paper` resolved to "a medium for written communication" and the page's to
  "a material made of cellulose pulp" — Wu-Palmer between two senses of the
  same word is 0.286, while an unrelated ticket whose `server` resolved to
  "utensil used in serving food or drink" scored 0.706, because Wu-Palmer
  rates any two artifacts highly. A serving dish outranked the query's own
  word.

  `conceptSimilarity` now returns 1 for the same lemma as well as the same
  synset, which is Mihalcea, Corley & Strapparava's own formulation — a word
  present in both texts is a full match, and taxonomy distance is the fallback
  for words that differ. On the JIRA case the relevant/irrelevant gap widened
  from 0.12 to 0.30. **Measured A/B on a 22-page dev.to crawl it changed
  almost nothing** (separation 0.298 → 0.301, no page moving more than 0.007),
  which is the expected shape: long articles give Lesk enough context that the
  two ends already agree, so this repairs the short-document case without
  disturbing the one that worked.

  A candidate second change — honouring the expansion's 1.0/0.6/0.3 decay in
  the page→query direction, where it is currently ignored — was measured and
  **rejected**: alone it collapsed the JIRA separation to 0.005, because it
  penalises a relevant page's high-similarity, low-weight matches hardest. It
  is recorded here because it is the obvious "correct" fix and it is wrong.

- Wu-Palmer's floor inside the artifact subtree remains unaddressed: any two
  artifacts score 0.7+. Escaping it needs an information-content measure (Lin,
  Jiang-Conrath) and corpus statistics the crawl does not keep.

### Risks
- **The platform makes outbound requests to third parties under its own
  name.** Mitigated by robots.txt, an identifying User-Agent, a per-host
  delay, a concurrency cap, and same-registrable-domain scoping by default.
- **The seed URL is untrusted input fetched from the platform's own network
  position** — textbook SSRF. Mitigated by refusing private, loopback and
  link-local addresses **in the dialer**, on the resolved IP: a hostname check
  is defeated by a DNS record pointing at 127.0.0.1, and a pre-flight
  resolution is defeated by DNS rebinding.
- **Cost per run is unbounded in embeddings**, not just in sense lookups:
  every stored page is chunked and embedded. `max_pages` is the ceiling, and
  the content hash keeps repeat runs cheap.
- **Single replica.** The scheduler has no leader election, matching the
  platform's existing sweeps. Two orchestrators would double-queue; the
  runner's status check makes that harmless but wasteful.
- Runs accumulate without bound, like workflow executions. No retention in v1.
