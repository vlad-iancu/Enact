package wsd

// DefaultAlpha weights the semantic half of the score against the lexical
// half. Semantic similarity is the reason this feature exists — it is what
// finds a page that never uses the query's words — so it leads; BM25 is the
// correction for everything the sense inventory has never heard of.
const DefaultAlpha = 0.7

// Score is a document's relevance to a query, and its parts.
//
// The parts are kept because the total alone is unreadable. A page scoring
// 0.4 entirely on BM25 is a different thing from one scoring 0.4 entirely on
// semantics — the first repeats the query's words without being about them,
// the second is about them in other words — and only the breakdown tells the
// difference. It is recorded on every node of the crawl report for that
// reason.
type Score struct {
	Total    float64 `json:"total"`
	Semantic float64 `json:"semantic"`
	Lexical  float64 `json:"lexical"`
	// Coverage is the fraction of the query the semantic half could judge —
	// see wsd.Coverage. Recorded because it explains the other two: a
	// coverage of 0.2 means Semantic was computed from a fifth of the query
	// and the total is mostly BM25, which is worth knowing before concluding
	// anything from a low score.
	Coverage float64 `json:"coverage"`
}

// Combine blends the two halves, weighting the semantic one by how much of
// the query it could actually judge.
//
// Alpha is the intended balance; coverage is how much of that is deliverable.
// A query whose senses are all in WordNet has coverage 1 and gets exactly the
// configured blend. A query of pure domain jargon has coverage 0 and falls
// back to BM25 alone — which is right, because its semantic score is not low,
// it is undefined, and letting a meaningless 0 through at alpha would push
// every page below the crawl's threshold equally.
//
// Alpha outside [0,1] falls back to the default rather than producing a score
// outside it, which would break the frontier's threshold comparison.
func Combine(semantic, lexical, alpha, coverage float64) Score {
	if alpha < 0 || alpha > 1 || alpha != alpha { // NaN fails every comparison
		alpha = DefaultAlpha
	}
	if coverage < 0 || coverage > 1 || coverage != coverage {
		coverage = 0
	}
	// The semantic half's influence is its intended share, scaled by the
	// share of the query it can speak about. What it gives up goes to BM25,
	// which can speak about all of it.
	effective := alpha * coverage
	return Score{
		Total:    effective*semantic + (1-effective)*lexical,
		Semantic: semantic,
		Lexical:  lexical,
		Coverage: coverage,
	}
}
