# ADR-0024: Names are recognised from evidence, and weighted above ordinary words

**Date**: 2026-09-01
**Status**: accepted (reverses the pure-Go constraint in [ADR-0022](0022-focused-crawling-with-knowledge-based-relevance.md))
**Deciders**: platform owner, Claude

## Context

A crawl for `"opensearch database documentation, syntax and query language"`
returned an article about building a document search tool with React and
Supabase. It never mentions OpenSearch. It matched four of the six words and,
at 1771 words to a real OpenSearch article's 719, beat that article on the
lexical half: 0.706 against 0.517.

Two things were wrong. Every query word weighed the same, so the one word that
decides relevance counted as much as `query`. And the crawler had no notion of
a name at all: entity extraction was off, `Term` had no such field, and
`NNP`/`NNPS` were folded into plain nouns.

An earlier attempt made it worse. Treating "absent from WordNet" as "is a name"
also marked `rebalancing` and every typo a user made — `documetation` and
`databse` were weighted triple, and because that weight enters BM25's
normalising ceiling, a typo dragged down the score of every page in the crawl.

## Decision

A term is a name on **evidence from the text**, never from a dictionary's
silence:

1. an entity extractor found one there;
2. the tagger called it a capitalised proper noun, away from the first token;
3. it is *spelled* like one — a capital inside the word (`OpenSearch`, `gRPC`),
   letters mixed with digits (`IPv6`), an all-capital run (`ORM`);
4. **the seed pages say so** — names are harvested from the pages a crawl was
   pointed at and carried to the query.

Names weigh `CRAWL_ENTITY_WEIGHT` (3) against 1, and a page mentioning **none**
of the query's names keeps `CRAWL_NAME_MISS_PENALTY` (0.3) of its score.

The extractor is a BERT token classifier run through ONNX Runtime — **the first
native dependency in the tree**, reversing ADR-0022's pure-Go choice. It is
optional (`NER_ENABLED=false`), `dlopen`ed only when enabled, so a deployment
that leaves it off links against nothing.

## Alternatives considered

### Alternative 1: Weighting alone, no name requirement
- **Pros**: one number, no new failure mode.
- **Cons**: a nudge, not a filter. One name at weight 3 beside five ordinary
  words is 37% of a query, so a long page saturating the other 63% still wins.
  Measured, the weighting cost the offending page 0.025.
- **Why not**: it did not work.

### Alternative 2: prose's built-in extractor only
- **Pros**: already a dependency, no model to ship.
- **Cons**: on a real crawled page it returned 68 "names" including `'re`,
  `and/or`, `every` and `copied`. Under seed harvesting, that furniture becomes
  query vocabulary weighted 3×.
- **Why not**: precision. The ONNX model returned 7 on the same page, all
  genuine.

### Alternative 3: A declared-names list per crawl
- **Pros**: exact, no model, and the only option that catches `kafka` or
  `python` in a lowercase query.
- **Cons**: needs the user to know and maintain it.
- **Why not**: not chosen *instead* — it remains the best answer for names that
  are also ordinary words, and is still unbuilt.

## Consequences

### Positive
- The wrong-product page went from `stored 0.528` to `rejected 0.158`, and the
  crawl became more selective (24 stored → 11) rather than merely reordered.
- Coverage was corrected alongside: it counted only concepts that *exist*, so a
  word with no sense was absent from the fraction and coverage read 1.00 while
  the semantic half was blind to the subject of the query.

### Negative
- **A native dependency and a 109 MB artefact.** `make ner-model` fetches the
  model and the runtime into `dist/`; a fresh checkout needs it before
  `NER_ENABLED=true` will work.
- Inference costs ~70 ms per document. Invisible at crawl scale because it runs
  on the seed pages, not on every page fetched.

### Risks
- **The model is cased and newswire-trained, with no software class.** It reads
  lowercase names no better than the spelling rules do, and misses `faiss` and
  `algolia` which a mid-word capital gives away for free. It corroborates the
  rules; it does not extend them. Measured on a real crawl it changed nothing —
  its value is keeping page furniture out of the harvest, which shows up as an
  absence of future weirdness rather than a visible gain.
- `0.3` for the name-miss penalty is a judgement, not a derivation. Raise it
  toward 1 if legitimate pages start dropping.
