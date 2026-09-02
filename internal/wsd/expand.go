package wsd

import "context"

// Expansion weights by distance from a query sense.
//
// The decay encodes how quickly meaning drifts along the graph: a synonym is
// the same concept and keeps full weight; a parent or child is closely
// related; two hops out is already a different topic that merely rhymes with
// the query, and is worth keeping only as weak evidence.
const (
	WeightSynonym    = 1.0
	WeightOneHop     = 0.6
	WeightTwoHops    = 0.3
	WeightDerivation = 0.6
)

// MaxExpansionHops is where the walk stops. Three hops in WordNet reaches
// "entity" from almost anywhere, at which point the expansion describes the
// language rather than the query.
const MaxExpansionHops = 2

// Bounds on the walk's breadth.
//
// Depth alone does not bound an expansion, because fan-out is wildly uneven:
// "river" has around a hundred hyponyms and "sea" nearly as many, so two hops
// from a six-word query reaches several hundred senses while two hops from a
// narrow technical term reaches a dozen. Left unbounded the broad terms
// swamp the narrow ones, which is the opposite of what a focused crawl wants
// — the narrow terms are the ones that identify the topic.
const (
	// DefaultMaxFanout caps how many relations are followed out of one sense.
	DefaultMaxFanout = 20
	// DefaultMaxExpansion caps the whole result. Beyond this, terms are being
	// added that no relevant page would contain.
	DefaultMaxExpansion = 250
)

// ExpandOptions bounds the walk.
type ExpandOptions struct {
	MaxHops      int
	MaxFanout    int
	MaxExpansion int
}

// DefaultExpandOptions are the shipped bounds.
var DefaultExpandOptions = ExpandOptions{
	MaxHops:      MaxExpansionHops,
	MaxFanout:    DefaultMaxFanout,
	MaxExpansion: DefaultMaxExpansion,
}

func (o ExpandOptions) normalized() ExpandOptions {
	if o.MaxHops <= 0 {
		o.MaxHops = MaxExpansionHops
	}
	if o.MaxFanout <= 0 {
		o.MaxFanout = DefaultMaxFanout
	}
	if o.MaxExpansion <= 0 {
		o.MaxExpansion = DefaultMaxExpansion
	}
	return o
}

// WeightedSense is one sense in the expanded query, and why it is there.
type WeightedSense struct {
	SynsetID string `json:"synset_id"`
	// Lemmas are what BM25 matches against; the semantic half uses the id.
	Lemmas   []string `json:"lemmas,omitempty"`
	Relation string   `json:"relation"`
	Hops     int      `json:"hops"`
	Weight   float64  `json:"weight"`
	// WordNetKey carries the taxonomy key so similarity need not re-resolve
	// the synset.
	WordNetKey string `json:"wordnet_key,omitempty"`
	Gloss      string `json:"gloss,omitempty"`
}

// Expand walks out from the query's disambiguated senses, collecting related
// senses with decaying weights.
//
// This is what turns a short query into something with enough surface to
// match a page: "otter habitat" alone matches almost nothing, but expanded
// through hypernyms ("mammal", "carnivore"), hyponyms ("sea otter") and
// derivations it matches the vocabulary a page on the subject actually uses.
//
// A sense reached by several paths keeps its BEST weight — the shortest path
// is the truest measure of how related it is, and summing would let a
// well-connected but irrelevant hub outrank a direct hypernym.
func Expand(ctx context.Context, inv Inventory, senses []Sense, opts ExpandOptions) ([]WeightedSense, error) {
	opts = opts.normalized()
	best := make(map[string]WeightedSense)
	// record keeps the strongest claim about a synset seen so far.
	record := func(ws WeightedSense) bool {
		prior, seen := best[ws.SynsetID]
		if seen && prior.Weight >= ws.Weight {
			return false
		}
		best[ws.SynsetID] = ws
		return true
	}

	// Hop 0: the query's own senses.
	frontier := make([]Synset, 0, len(senses))
	for _, sense := range senses {
		if sense.SynsetID == "" {
			continue
		}
		s, err := inv.Synset(ctx, sense.SynsetID)
		if err != nil {
			return collect(best, opts.MaxExpansion), err
		}
		// A resolved sense IS the concept, so it enters at full weight under
		// the synonym label — its lemmas are the query's own synonyms.
		record(WeightedSense{
			SynsetID: sense.SynsetID, Lemmas: s.Lemmas, Relation: "synonym",
			Hops: 0, Weight: WeightSynonym, WordNetKey: sense.WordNetKey, Gloss: s.Gloss,
		})
		frontier = append(frontier, s)
	}

	for hop := 1; hop <= opts.MaxHops; hop++ {
		next := make([]Synset, 0, len(frontier)*4)
		for _, node := range frontier {
			followed := 0
			for _, rel := range node.Relations {
				// Instances are named entities, not kinds. Following them
				// turns a topical expansion into a gazetteer.
				if rel.Type == RelationInstance {
					continue
				}
				if followed >= opts.MaxFanout {
					break
				}
				related, err := glossOf(ctx, inv, rel.Target)
				if err != nil {
					// Out of budget, or the inventory is unreachable. What has
					// been collected so far is a valid, smaller expansion —
					// the caller decides whether to proceed with it.
					return collect(best, opts.MaxExpansion), err
				}
				if related.ID == "" {
					continue
				}
				followed++
				ws := WeightedSense{
					SynsetID:   related.ID,
					Lemmas:     related.Lemmas,
					Relation:   rel.Type,
					Hops:       hop,
					Weight:     hopWeight(hop, rel.Type),
					WordNetKey: NormalizeWordNetKey(related.WordNetKey),
					Gloss:      related.Gloss,
				}
				// Only walk on from senses this hop actually improved, so a
				// node reached cheaply earlier is not re-expanded.
				if record(ws) && hop < opts.MaxHops {
					next = append(next, related)
				}
			}
		}
		frontier = next
		if len(frontier) == 0 || len(best) >= opts.MaxExpansion {
			break
		}
	}
	return collect(best, opts.MaxExpansion), nil
}

// hopWeight is the decay schedule. A derivationally related form ("crawl" ->
// "crawler") is the same concept in another part of speech, so it keeps a
// close weight regardless of how many hops away it was found.
func hopWeight(hop int, relation string) float64 {
	if relation == RelationDerivation {
		return WeightDerivation
	}
	switch hop {
	case 0:
		return WeightSynonym
	case 1:
		return WeightOneHop
	default:
		return WeightTwoHops
	}
}

func collect(m map[string]WeightedSense, limit int) []WeightedSense {
	out := make([]WeightedSense, 0, len(m))
	for _, ws := range m {
		out = append(out, ws)
	}
	// Deterministic order: strongest first, ties broken by id so a report is
	// reproducible and two runs can be diffed.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].Weight > out[j-1].Weight ||
				(out[j].Weight == out[j-1].Weight && out[j].SynsetID < out[j-1].SynsetID) {
				out[j], out[j-1] = out[j-1], out[j]
				continue
			}
			break
		}
	}
	// Truncating AFTER sorting keeps the strongest senses, so the cap costs
	// the weakest two-hop drift rather than an arbitrary slice.
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ExpandedTerms flattens an expansion into weighted lemmas, which is the form
// BM25 consumes. A lemma reached by several senses keeps its highest weight.
func ExpandedTerms(expansion []WeightedSense) map[string]float64 {
	out := make(map[string]float64, len(expansion)*2)
	for _, ws := range expansion {
		for _, lemma := range ws.Lemmas {
			key := normalizeLemma(lemma)
			if key == "" {
				continue
			}
			if prior, ok := out[key]; !ok || ws.Weight > prior {
				out[key] = ws.Weight
			}
		}
	}
	return out
}

// DefaultEntityWeight is what a name counts for in the lexical half.
//
// Three rather than one, and the number is a judgement rather than a
// derivation. The reasoning that fixes its order of magnitude: an ordinary
// query word is scored twice — once semantically through its synset, once
// lexically — while a name has no synset and is scored once. Restoring parity
// argues for two. Going a little beyond that reflects what a name actually
// means in a query: "opensearch database documentation" is not a request for
// six equally weighted topics, it is a request about OpenSearch, and the other
// five words say which part of it.
//
// Measured on that query, at 1.0 a Postgres page matched five of six terms and
// scored as well as a real OpenSearch page.
const DefaultEntityWeight = 3.0

// QueryTerms is the vocabulary BM25 matches a page against: the expanded
// query, plus the query's own words, with names weighted above the rest.
//
// Including the query's own words is not a detail. ExpandedTerms alone derives
// every term from a resolved sense, so a word the inventory has never heard of
// contributes NOTHING to the lexical half — and the words a dictionary has
// never heard of are product names, jargon and acronyms, which is to say the
// most discriminating words in any technical query. Measured: a crawl for
// "opensearch indices, security, syntax and usage" could not lexically match
// the word "opensearch", because WordNet has no entry for it. The lexical
// score collapsed to noise and a flat semantic score was left ranking the
// crawl, which duly preferred the site's help pages to its OpenSearch
// articles.
//
// BM25 needs no sense inventory to do its job — matching a word is what it is
// for, and it is precisely the half of the score that is supposed to cover
// what the sense inventory cannot. Withholding the query's own words from it
// gave up that cover exactly when it was most needed.
//
// Names then enter at entityWeight rather than 1, because a query names one
// thing and describes it with several: without that, a page about the wrong
// product but the right subject matches everything except the only word that
// mattered. Weights never fall below what the expansion already assigned.
func QueryTerms(terms []Term, expansion []WeightedSense, entityWeight float64) map[string]float64 {
	if entityWeight <= 0 {
		entityWeight = DefaultEntityWeight
	}
	out := ExpandedTerms(expansion)
	for _, term := range terms {
		key := normalizeLemma(term.Lemma)
		if key == "" {
			continue
		}
		weight := 1.0
		if term.Entity {
			weight = entityWeight
		}
		if out[key] < weight {
			out[key] = weight
		}
	}
	return out
}
