package wsd

import "math"

// Concept is one disambiguated sense with the weight it carries in a text.
type Concept struct {
	SynsetID   string
	WordNetKey string
	Lemma      string
	// Weight is the idf of the lemma: how much this concept distinguishes
	// its text from every other text.
	Weight float64
}

// ConceptsFromSenses turns disambiguated senses into weighted concepts,
// dropping the ones that resolved to nothing.
func ConceptsFromSenses(senses []Sense, idf func(lemma string) float64) []Concept {
	out := make([]Concept, 0, len(senses))
	seen := make(map[string]bool, len(senses))
	for _, sense := range senses {
		if sense.SynsetID == "" || seen[sense.SynsetID] {
			continue
		}
		seen[sense.SynsetID] = true
		weight := 1.0
		if idf != nil {
			weight = idf(sense.Term.Lemma)
		}
		out = append(out, Concept{
			SynsetID:   sense.SynsetID,
			WordNetKey: NormalizeWordNetKey(sense.WordNetKey),
			Lemma:      sense.Term.Lemma,
			Weight:     weight,
		})
	}
	return out
}

// ConceptsFromExpansion turns an expanded query into concepts, using the
// expansion weight in place of idf.
//
// Using the expansion weight is the point of expanding: a hypernym two hops
// out should influence the score less than the query's own senses, and that
// is expressed here rather than by dropping it.
func ConceptsFromExpansion(expansion []WeightedSense) []Concept {
	out := make([]Concept, 0, len(expansion))
	for _, ws := range expansion {
		lemma := ""
		if len(ws.Lemmas) > 0 {
			lemma = normalizeLemma(ws.Lemmas[0])
		}
		out = append(out, Concept{
			SynsetID:   ws.SynsetID,
			WordNetKey: NormalizeWordNetKey(ws.WordNetKey),
			Lemma:      lemma,
			Weight:     ws.Weight,
		})
	}
	return out
}

// Similarity is the Mihalcea, Corley & Strapparava (2006) text-to-text
// semantic similarity of two concept sets, in [0,1].
//
// For each concept in A, find the concept in B most similar to it, weight
// that maximum by the concept's own importance, and average over A. Do the
// same from B to A and take the mean of the two directions.
//
// The symmetry matters and is not cosmetic. One-directional similarity is
// biased by length: a three-word query is trivially "contained" in a long
// page, so query-to-page alone scores almost everything highly. Averaging
// with page-to-query asks the harder question — is the page ALSO about the
// query — and it is that direction which separates a page on the topic from a
// page that merely mentions it.
func Similarity(tax *Taxonomy, a, b []Concept) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	forward := directional(tax, a, b)
	backward := directional(tax, b, a)
	return (forward + backward) / 2
}

// directional computes the idf-weighted average of each concept's best match
// in the other set, over the concepts that CAN be matched at all.
func directional(tax *Taxonomy, from, to []Concept) float64 {
	var weighted, weights float64
	for _, c := range from {
		// Unmeasurable concepts are skipped entirely — see measurable. They
		// must not enter the denominator: scoring 0 for a concept the measure
		// cannot express is absence of evidence, not evidence of absence, and
		// counting it would penalise a query for being specific.
		if !measurable(c, to) {
			continue
		}
		best := 0.0
		for _, other := range to {
			if s := conceptSimilarity(tax, c, other); s > best {
				best = s
				if best == 1 {
					break
				}
			}
		}
		weight := c.Weight
		if weight <= 0 {
			// A concept with no weight would silently vanish from both the
			// numerator and denominator; treating it as neutral keeps it
			// counted.
			weight = 1
		}
		weighted += best * weight
		weights += weight
	}
	if weights == 0 {
		return 0
	}
	return weighted / weights
}

// measurable reports whether a concept could match anything in the target
// set under conceptSimilarity.
//
// This is the seam between the two inventories. A crawl's query is
// disambiguated against BabelNet, which knows named entities, products and
// domain jargon that WordNet has never heard of — that richer vocabulary is
// the whole reason BabelNet is used. But those senses have no WordNet offset,
// so Wu-Palmer has nothing to measure them on, and they can only match by
// exact synset identity, which a WordNet-derived page concept never has.
//
// Left in the average they were actively harmful: measured on a page that
// genuinely was about otters, adding three BabelNet-only senses to the query
// dropped the semantic score from 0.993 to 0.732. Enriching the query made
// the same page look less relevant.
//
// They are not discarded from the relevance function — their lemmas are in
// the expanded query and BM25 matches them directly, which is exactly the
// division of labour the two halves exist for. See Coverage for how the
// blend accounts for the part semantics cannot reach.
func measurable(c Concept, against []Concept) bool {
	if c.WordNetKey != "" {
		return true
	}
	if c.SynsetID == "" {
		return false
	}
	for _, other := range against {
		if other.SynsetID == c.SynsetID {
			return true
		}
	}
	return false
}

// Coverage is the fraction of a concept set's weight that the semantic
// measure can speak about at all, in [0,1].
//
// It is how the blend stays honest when the query outruns WordNet. A query
// made entirely of domain jargon has coverage 0: its semantic score is not
// low, it is undefined, and weighting a meaningless 0 at alpha would drag
// every page down uniformly. Combine multiplies alpha by this, so the
// semantic half earns influence in proportion to how much of the query it
// can actually judge, and a fully unmeasurable query falls back to BM25
// alone.
func Coverage(concepts, against []Concept) float64 {
	var total, covered float64
	for _, c := range concepts {
		weight := c.Weight
		if weight <= 0 {
			weight = 1
		}
		total += weight
		if measurable(c, against) {
			covered += weight
		}
	}
	if total == 0 {
		return 0
	}
	return covered / total
}

// conceptSimilarity scores two concepts. Identical senses are 1 by
// definition — including senses BabelNet knows but WordNet does not, which
// is the only signal available for named entities.
func conceptSimilarity(tax *Taxonomy, a, b Concept) float64 {
	if a.SynsetID != "" && a.SynsetID == b.SynsetID {
		return 1
	}
	// The same word in both texts is a full match, even when the two ends
	// disambiguated it differently.
	//
	// Requiring an exact SYNSET match is a stricter reading than Mihalcea,
	// Corley & Strapparava, whose measure takes a word present in both texts
	// as similarity 1 and falls back to taxonomy distance only for words that
	// differ. The strictness was doing real damage. Measured on a two-word
	// JIRA ticket: the query's "paper" resolved to "a medium for written
	// communication" and the page's to "a material made of cellulose pulp",
	// scoring 0.286 — while an unrelated page whose "server" resolved to
	// "utensil used in serving food or drink" scored 0.706, because Wu-Palmer
	// rates any two artifacts highly. A serving dish outranked the query's own
	// word.
	//
	// Short documents give Lesk almost no context, so the two ends disagreeing
	// about a word they BOTH contain is common, and it is the one case where
	// the evidence is unambiguous whatever the senses say.
	if a.Lemma != "" && a.Lemma == b.Lemma {
		return 1
	}
	if tax == nil {
		return 0
	}
	return tax.Similarity(a.WordNetKey, b.WordNetKey)
}

// IDF builds an inverse-document-frequency function over a growing corpus.
//
// A crawl discovers its corpus as it goes, so idf is necessarily an estimate
// from the pages seen so far. That is fine for ranking within one run — every
// page is scored against the same estimate at the moment it is scored — but
// it does mean the first few pages of a run are scored on thin evidence.
type IDF struct {
	docs  int
	seen  map[string]int
	total map[string]bool
}

func NewIDF() *IDF {
	return &IDF{seen: map[string]int{}, total: map[string]bool{}}
}

// Observe records one document's distinct lemmas.
func (i *IDF) Observe(terms []Term) {
	i.docs++
	distinct := make(map[string]bool, len(terms))
	for _, term := range terms {
		distinct[term.Lemma] = true
	}
	for lemma := range distinct {
		i.seen[lemma]++
	}
}

// Docs is how many documents have been observed.
func (i *IDF) Docs() int { return i.docs }

// Score is the smoothed idf of a lemma. An unseen lemma gets the maximum,
// because a word no document has used is maximally distinguishing.
func (i *IDF) Score(lemma string) float64 {
	if i == nil || i.docs == 0 {
		return 1
	}
	n := i.seen[lemma]
	// Standard smoothed idf, always positive: log((N + 1) / (n + 1)) + 1.
	return math.Log(float64(i.docs+1)/float64(n+1)) + 1
}
