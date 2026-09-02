package wsd

import "math"

// BM25 parameters, at the values the literature settles on.
//
// K1 bounds how much repetition helps: term frequency saturates, so the
// twentieth mention of a word adds almost nothing over the tenth. B controls
// length normalisation: at 0.75 a long document is penalised for its length,
// but not fully, since a long document genuinely can be more relevant.
const (
	BM25K1 = 1.2
	BM25B  = 0.75
)

// BM25 scores documents against the expanded query lexically.
//
// It is the cheap half of the relevance function, and it earns its place by
// covering exactly what the semantic half cannot: proper nouns, technical
// terms, product names and anything else absent from the sense inventory. A
// page about a named framework may share no synsets with the query and still
// be plainly on topic because it repeats the query's words.
type BM25 struct {
	idf       *IDF
	totalLen  int
	documents int
}

func NewBM25(idf *IDF) *BM25 { return &BM25{idf: idf} }

// Observe records a document's length, which feeds the average the length
// normalisation is relative to.
func (b *BM25) Observe(terms []Term) {
	b.documents++
	b.totalLen += len(terms)
}

// AverageLength is the mean document length seen so far, or 1 before
// anything has been observed.
func (b *BM25) AverageLength() float64 {
	if b.documents == 0 || b.totalLen == 0 {
		return 1
	}
	return float64(b.totalLen) / float64(b.documents)
}

// CoreQueryTerms is how many of the query's strongest terms define a perfect
// lexical match.
//
// The normalisation constant, and it matters more than it looks. Raw BM25 is
// unbounded, so it has to be mapped into [0,1] to be blended with a
// similarity and compared against a threshold — but normalising by "every
// query term matching at saturation" makes the ceiling unreachable, because
// an expanded query has hundreds of terms and no real document contains them
// all. Measured on a page genuinely about the topic, that normalisation
// produced a lexical score of 0.018: the whole lexical half of the blend was
// inert, contributing under a hundredth of the total, and BM25 could not do
// the job it is there for.
//
// A document that is about the query contains the query's CORE vocabulary,
// not its two-hop expansion. Normalising against the strongest handful makes
// the ceiling something a good document actually reaches.
const CoreQueryTerms = 10

// Score rates a document against a weighted query term set, normalised to
// [0,1].
func (b *BM25) Score(doc []Term, query map[string]float64) float64 {
	if len(doc) == 0 || len(query) == 0 {
		return 0
	}
	freq := make(map[string]int, len(doc))
	for _, term := range doc {
		freq[term.Lemma]++
	}
	docLen := float64(len(doc))
	avg := b.AverageLength()

	var score float64
	for lemma, weight := range query {
		f := float64(freq[lemma])
		if f == 0 {
			continue
		}
		idf := 1.0
		if b.idf != nil {
			idf = b.idf.Score(lemma)
		}
		denom := f + BM25K1*(1-BM25B+BM25B*docLen/avg)
		score += weight * idf * (f * (BM25K1 + 1) / denom)
	}
	ceiling := b.ceiling(query)
	if ceiling == 0 {
		return 0
	}
	return math.Min(1, score/ceiling)
}

// ceiling is what the CoreQueryTerms strongest query terms would contribute
// if a document matched them all at saturation.
func (b *BM25) ceiling(query map[string]float64) float64 {
	type scored struct {
		weight, idf float64
	}
	terms := make([]scored, 0, len(query))
	for lemma, weight := range query {
		idf := 1.0
		if b.idf != nil {
			idf = b.idf.Score(lemma)
		}
		terms = append(terms, scored{weight, idf})
	}
	// Partial selection of the top CoreQueryTerms by contribution. The set is
	// a few hundred at most and this runs once per page.
	n := CoreQueryTerms
	if n > len(terms) {
		n = len(terms)
	}
	var total float64
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(terms); j++ {
			if terms[j].weight*terms[j].idf > terms[best].weight*terms[best].idf {
				best = j
			}
		}
		terms[i], terms[best] = terms[best], terms[i]
		total += terms[i].weight * terms[i].idf * (BM25K1 + 1)
	}
	return total
}
