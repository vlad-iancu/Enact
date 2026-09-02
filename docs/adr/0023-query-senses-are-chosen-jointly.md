# ADR-0023: Query senses are chosen jointly, by ant colony optimisation

**Date**: 2026-09-01
**Status**: accepted (supersedes the disambiguation method in [ADR-0022](0022-focused-crawling-with-knowledge-based-relevance.md))
**Deciders**: platform owner, Claude

## Context

ADR-0022 chose extended Lesk (Banerjee & Pedersen 2002) to read a crawl's
query: score each word's candidate senses against the surrounding words, keep
the best. That decides each word **independently**, and independent decisions
have no way to notice they contradict each other.

Measured on `"opensearch indices, security, syntax and usage"` it returned the
software sense of `opensearch`, the *semiotics* sense of `index`, the
*collateral* sense of `security` and the *habit* sense of `usage` — four senses
that co-occur in no text ever written. Each was defensible alone. The expansion
inherited them, BM25 runs over the expansion, and so the site's terms-of-service
page beat its OpenSearch tag page on the lexical half: 0.21 against 0.07. Both
halves of the score are downstream of disambiguation, and lowering `alpha`
cannot rescue a query that was misunderstood.

## Decision

The query is disambiguated by **ant colony optimisation over the extended Lesk
objective**: whole assignments are scored by how much their senses agree with
each other and with the surrounding text, and the colony searches that space
rather than deciding word by word.

Pages keep the greedy method — there are hundreds per run, and a page is its
own context.

## Alternatives considered

### Alternative 1: Keep greedy Lesk and lower alpha
- **Pros**: nothing to build.
- **Cons**: BM25 runs over the *expanded* query, so it inherits the same wrong
  senses. Measured, it did.
- **Why not**: it treats the symptom in the half that was already poisoned.

### Alternative 2: Exhaustive search of the assignment space
- **Pros**: exact.
- **Cons**: the product of every word's sense count — 10^20 for a twenty-word
  query at ten senses each.
- **Why not**: intractable. Though it *is* tractable for a five-word query, and
  that is exactly what `make wsd-diag` uses to check the colony's answer.

### Alternative 3: A neural WSD model
- **Pros**: better accuracy than any Lesk variant.
- **Cons**: a model to ship, a runtime to depend on, and inference per query.
- **Why not**: not at the time. (Partly revisited in
  [ADR-0024](0024-names-are-recognised-from-evidence.md), which does add a
  model — for names, not senses.)

## Consequences

### Positive
- Senses that support one another win, which is what "understanding the query"
  means here.
- **No extra inventory lookups.** Every candidate and its extended gloss is
  fetched once, before the colony starts — the same fetches greedy Lesk makes.
  A metered dictionary sees no difference. This is what made it affordable.
- Deterministic: a fixed seed, so two runs of one crawl produce the same
  report and a difference between reports means the corpus changed.

### Negative
- A second algorithm to understand, with parameters (ants, cycles, evaporation)
  that look tunable and mostly are not — see the risk below.
- Slower per query, though invisible beside fetching.

### Risks
- **The colony is not usually the problem, and its knobs invite fiddling.**
  Every defect found since has been in the *objective*: function words scoring
  as agreement, WordNet's example sentences leaking into the comparison, and
  long glosses winning on bulk. In each case the colony was hitting the exact
  optimum of a scoring function that was wrong. `make wsd-diag` exists to
  settle that question first — it brute-forces the objective and says whether
  the search or the objective is at fault, and it has never yet been the
  search.
