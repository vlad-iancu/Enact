package wsd

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// graphInventory is a small semantic network:
//
//	vehicle
//	  └── car ── (meronym) ── wheel
//	        └── sedan
//
// otter is disconnected, for the "unrelated concepts" cases.
func graphInventory() *fakeInventory {
	syn := map[string]Synset{
		"s:car": {
			ID: "s:car", POS: POSNoun, Lemmas: []string{"car", "automobile"},
			Gloss:      "a motor vehicle with four wheels",
			WordNetKey: "wn:02958343n",
			Relations: []Relation{
				{Target: "s:vehicle", Type: RelationHypernym},
				{Target: "s:sedan", Type: RelationHyponym},
				{Target: "s:wheel", Type: RelationMeronym},
			},
		},
		"s:vehicle": {
			ID: "s:vehicle", POS: POSNoun, Lemmas: []string{"vehicle"},
			Gloss:      "a conveyance that transports people",
			WordNetKey: "wn:04524313n",
			Relations:  []Relation{{Target: "s:conveyance", Type: RelationHypernym}},
		},
		"s:sedan": {
			ID: "s:sedan", POS: POSNoun, Lemmas: []string{"sedan", "saloon"},
			Gloss: "a car seating four or more", WordNetKey: "wn:04166281n",
		},
		"s:wheel": {
			ID: "s:wheel", POS: POSNoun, Lemmas: []string{"wheel"},
			Gloss: "a simple machine consisting of a circular frame",
		},
		"s:conveyance": {
			ID: "s:conveyance", POS: POSNoun, Lemmas: []string{"conveyance", "transport"},
			Gloss: "something that serves to carry people or goods",
		},
		"s:otter": {
			ID: "s:otter", POS: POSNoun, Lemmas: []string{"otter"},
			Gloss: "a freshwater carnivorous mammal",
		},
	}
	return &fakeInventory{
		senses: map[string][]Synset{
			POSNoun + ":car":   {syn["s:car"]},
			POSNoun + ":otter": {syn["s:otter"]},
		},
		synsets: syn,
	}
}

func TestExpandDecaysWithDistance(t *testing.T) {
	inv := graphInventory()
	senses := []Sense{{Term: Term{Lemma: "car", POS: POSNoun}, SynsetID: "s:car"}}
	expansion, err := Expand(context.Background(), inv, senses, DefaultExpandOptions)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	got := map[string]WeightedSense{}
	for _, ws := range expansion {
		got[ws.SynsetID] = ws
	}

	cases := []struct {
		id     string
		weight float64
		hops   int
	}{
		{"s:car", WeightSynonym, 0},    // the sense itself
		{"s:vehicle", WeightOneHop, 1}, // hypernym, one hop
		{"s:sedan", WeightOneHop, 1},   // hyponym, one hop
		{"s:wheel", WeightOneHop, 1},   // meronym, one hop
		{"s:conveyance", WeightTwoHops, 2},
	}
	for _, tc := range cases {
		ws, ok := got[tc.id]
		if !ok {
			t.Errorf("%s missing from the expansion", tc.id)
			continue
		}
		if ws.Weight != tc.weight {
			t.Errorf("%s weight = %.2f, want %.2f", tc.id, ws.Weight, tc.weight)
		}
		if ws.Hops != tc.hops {
			t.Errorf("%s hops = %d, want %d", tc.id, ws.Hops, tc.hops)
		}
	}
	// The walk must stop at two hops: nothing beyond conveyance.
	for _, ws := range expansion {
		if ws.Hops > MaxExpansionHops {
			t.Errorf("%s is %d hops out, beyond the %d-hop limit", ws.SynsetID, ws.Hops, MaxExpansionHops)
		}
	}
	// Strongest first, so a report reads in order of relevance.
	for i := 1; i < len(expansion); i++ {
		if expansion[i].Weight > expansion[i-1].Weight {
			t.Errorf("expansion is not sorted by weight: %v", expansion)
			break
		}
	}
}

func TestExpandKeepsBestWeightForRepeatedSense(t *testing.T) {
	// wheel is reachable at one hop from car, and would also be reachable at
	// two hops through vehicle if the graph looped. It must keep 0.6.
	inv := graphInventory()
	inv.synsets["s:vehicle"] = Synset{
		ID: "s:vehicle", POS: POSNoun, Lemmas: []string{"vehicle"},
		Gloss: "a conveyance that transports people",
		Relations: []Relation{
			{Target: "s:conveyance", Type: RelationHypernym},
			{Target: "s:wheel", Type: RelationMeronym}, // the second path
		},
	}
	expansion, err := Expand(context.Background(), inv,
		[]Sense{{Term: Term{Lemma: "car", POS: POSNoun}, SynsetID: "s:car"}}, DefaultExpandOptions)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, ws := range expansion {
		if ws.SynsetID == "s:wheel" && ws.Weight != WeightOneHop {
			t.Errorf("wheel kept weight %.2f, want the best (shortest-path) %.2f", ws.Weight, WeightOneHop)
		}
	}
}

func TestExpandedTermsFlattensToLemmas(t *testing.T) {
	inv := graphInventory()
	expansion, err := Expand(context.Background(), inv,
		[]Sense{{Term: Term{Lemma: "car", POS: POSNoun}, SynsetID: "s:car"}}, DefaultExpandOptions)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	terms := ExpandedTerms(expansion)
	if terms["car"] != WeightSynonym || terms["automobile"] != WeightSynonym {
		t.Errorf("query's own lemmas should carry full weight, got car=%.2f automobile=%.2f",
			terms["car"], terms["automobile"])
	}
	if terms["vehicle"] != WeightOneHop {
		t.Errorf("hypernym lemma weight = %.2f, want %.2f", terms["vehicle"], WeightOneHop)
	}
}

func TestSimilarityIsSymmetricAndSelfIsOne(t *testing.T) {
	a := []Concept{{SynsetID: "s:car", Weight: 1}, {SynsetID: "s:wheel", Weight: 1}}
	b := []Concept{{SynsetID: "s:otter", Weight: 1}}

	if got := Similarity(nil, a, a); got != 1 {
		t.Errorf("a text against itself = %.3f, want 1", got)
	}
	forward, backward := Similarity(nil, a, b), Similarity(nil, b, a)
	if forward != backward {
		t.Errorf("Similarity is not symmetric: %.4f vs %.4f", forward, backward)
	}
	if forward != 0 {
		t.Errorf("wholly unrelated concept sets = %.3f, want 0 without a taxonomy", forward)
	}
	if got := Similarity(nil, a, nil); got != 0 {
		t.Errorf("similarity against nothing = %.3f, want 0", got)
	}
}

// TestSimilarityIsLengthFair is why the measure is symmetric: a short query
// fully contained in a long, mostly-unrelated page must not score as highly
// as one against a page that is actually about it.
func TestSimilarityIsLengthFair(t *testing.T) {
	// Every concept carries a WordNet key, because that is what a page
	// concept always is: pages are disambiguated against the local WordNet,
	// so they are measurable by construction. Without keys these would be
	// skipped as unmeasurable (see measurable) and the length-fairness this
	// test is about would not apply to them.
	query := []Concept{{SynsetID: "s:car", WordNetKey: "n02958343", Weight: 1}}
	onTopic := []Concept{{SynsetID: "s:car", WordNetKey: "n02958343", Weight: 1}}
	mentionsInPassing := []Concept{
		{SynsetID: "s:car", WordNetKey: "n02958343", Weight: 1},
		{SynsetID: "s:otter", WordNetKey: "n02444819", Weight: 1},
		{SynsetID: "s:wheel", WordNetKey: "n04574999", Weight: 1},
		{SynsetID: "s:conveyance", WordNetKey: "n03100490", Weight: 1},
	}
	focused := Similarity(nil, query, onTopic)
	passing := Similarity(nil, query, mentionsInPassing)
	if !(focused > passing) {
		t.Errorf("focused page %.3f must outscore the one that merely mentions the topic %.3f",
			focused, passing)
	}
}

func TestSimilarityWeightsByImportance(t *testing.T) {
	// The matched concept is the important one, so the score should be high;
	// flipping the weights should drop it.
	// Keyed, for the same reason as TestSimilarityIsLengthFair: an
	// unmeasurable concept is skipped rather than weighted, so weighting only
	// means anything among concepts the measure can actually judge.
	matched := Concept{SynsetID: "s:car", WordNetKey: "n02958343"}
	unmatched := Concept{SynsetID: "s:otter", WordNetKey: "n02444819"}
	page := []Concept{{SynsetID: "s:car", WordNetKey: "n02958343", Weight: 1}}

	important := Similarity(nil, []Concept{
		{SynsetID: matched.SynsetID, WordNetKey: matched.WordNetKey, Weight: 10},
		{SynsetID: unmatched.SynsetID, WordNetKey: unmatched.WordNetKey, Weight: 1},
	}, page)
	unimportant := Similarity(nil, []Concept{
		{SynsetID: matched.SynsetID, WordNetKey: matched.WordNetKey, Weight: 1},
		{SynsetID: unmatched.SynsetID, WordNetKey: unmatched.WordNetKey, Weight: 10},
	}, page)
	if !(important > unimportant) {
		t.Errorf("matching the high-weight concept (%.3f) should beat matching the low-weight one (%.3f)",
			important, unimportant)
	}
}

// TestBabelNetOnlyConceptsDoNotPenalise is the regression for the flaw that
// made the richer vocabulary counterproductive.
//
// A crawl's query is disambiguated against BabelNet, which knows named
// entities and domain jargon WordNet does not. Those senses carry no WordNet
// offset, so Wu-Palmer cannot measure them — and while they were counted in
// the average's denominator, adding them dropped the score of a page that was
// genuinely about the topic from 0.993 to 0.732. Enriching the query made the
// same page look less relevant, which is precisely backwards.
func TestBabelNetOnlyConceptsDoNotPenalise(t *testing.T) {
	page := []Concept{{SynsetID: "n02444819", WordNetKey: "n02444819", Weight: 1}}
	measurableQuery := []Concept{
		{SynsetID: "bn:00059723n", WordNetKey: "n02444819", Weight: 1.0},
	}
	// The same query, enriched with senses only BabelNet knows.
	enriched := append(append([]Concept{}, measurableQuery...),
		Concept{SynsetID: "bn:03083790n", Lemma: "enhydra lutris", Weight: 0.6},
		Concept{SynsetID: "bn:01234567n", Lemma: "kelp forest ecosystem", Weight: 0.6},
	)

	plain := Similarity(nil, measurableQuery, page)
	rich := Similarity(nil, enriched, page)
	if rich < plain {
		t.Errorf("enriching the query lowered the score of the same page: %.4f -> %.4f", plain, rich)
	}

	// And the blend knows how much of the query it judged.
	if got := Coverage(enriched, page); math.Abs(got-1.0/2.2) > 1e-9 {
		t.Errorf("Coverage = %.4f, want the measurable share of the weight (1.0 of 2.2)", got)
	}
	if got := Coverage(measurableQuery, page); got != 1 {
		t.Errorf("a fully measurable query has coverage %.4f, want 1", got)
	}
	// A query with nothing measurable is undefined, not zero-scoring.
	jargon := []Concept{{SynsetID: "bn:03083790n", Weight: 1}, {SynsetID: "bn:01234567n", Weight: 1}}
	if got := Coverage(jargon, page); got != 0 {
		t.Errorf("an unmeasurable query has coverage %.4f, want 0", got)
	}
	if got := Similarity(nil, jargon, page); got != 0 {
		t.Errorf("an unmeasurable query scored %.4f; it should be 0 and carry no weight", got)
	}
}

// TestUnmeasurableConceptsStillMatchByIdentity: two concepts from the SAME
// inventory match on synset id even without a WordNet key, so the skip must
// not throw away a genuine match.
func TestUnmeasurableConceptsStillMatchByIdentity(t *testing.T) {
	entity := Concept{SynsetID: "bn:03083790n", Weight: 1}
	page := []Concept{{SynsetID: "bn:03083790n", Weight: 1}}
	if got := Similarity(nil, []Concept{entity}, page); got != 1 {
		t.Errorf("identical keyless concepts scored %.3f, want 1", got)
	}
	if got := Coverage([]Concept{entity}, page); got != 1 {
		t.Errorf("Coverage = %.3f; a concept the target set contains IS measurable", got)
	}
}

// TestMeasurableConceptsStillCountAgainstAPage: the skip must apply only to
// concepts that CANNOT be measured, never to ones that were measured and
// scored badly — otherwise an off-topic page would score as well as a
// relevant one.
func TestMeasurableConceptsStillCountAgainstAPage(t *testing.T) {
	tax := testTaxonomy(t)
	query := []Concept{
		{SynsetID: "bn:a", WordNetKey: "n02084071", Weight: 1}, // dog
		{SynsetID: "bn:b", WordNetKey: "n02121620", Weight: 1}, // cat
	}
	onTopic := []Concept{{SynsetID: "n02084071", WordNetKey: "n02084071", Weight: 1}}
	offTopic := []Concept{{SynsetID: "n09213565", WordNetKey: "n09213565", Weight: 1}} // river bank

	near, far := Similarity(tax, query, onTopic), Similarity(tax, query, offTopic)
	if !(near > far) {
		t.Errorf("an on-topic page (%.3f) must outscore an off-topic one (%.3f)", near, far)
	}
	if far == 0 {
		t.Error("an off-topic but measurable page should score low, not be skipped entirely")
	}
}

func TestIDFRewardsRareTerms(t *testing.T) {
	idf := NewIDF()
	// "the" in every document, "otter" in one.
	idf.Observe([]Term{{Lemma: "the"}, {Lemma: "otter"}})
	idf.Observe([]Term{{Lemma: "the"}, {Lemma: "car"}})
	idf.Observe([]Term{{Lemma: "the"}, {Lemma: "wheel"}})

	if idf.Docs() != 3 {
		t.Fatalf("Docs = %d, want 3", idf.Docs())
	}
	common, rare, unseen := idf.Score("the"), idf.Score("otter"), idf.Score("zzz")
	if !(rare > common) {
		t.Errorf("rare term idf %.3f must exceed common term idf %.3f", rare, common)
	}
	if !(unseen >= rare) {
		t.Errorf("unseen term idf %.3f must be at least the rarest seen %.3f", unseen, rare)
	}
	// Repeats within one document must not count twice.
	before := idf.Score("car")
	idf.Observe([]Term{{Lemma: "car"}, {Lemma: "car"}, {Lemma: "car"}})
	if after := idf.Score("car"); after >= before {
		t.Errorf("idf of car did not fall after another document used it: %.3f -> %.3f", before, after)
	}
	empty := NewIDF()
	if got := empty.Score("anything"); got != 1 {
		t.Errorf("idf with no corpus = %.3f, want the neutral 1", got)
	}
}

func TestBM25(t *testing.T) {
	idf := NewIDF()
	docA := []Term{{Lemma: "otter"}, {Lemma: "otter"}, {Lemma: "river"}}
	docB := []Term{{Lemma: "car"}, {Lemma: "wheel"}, {Lemma: "road"}}
	docC := []Term{{Lemma: "otter"}, {Lemma: "sea"}, {Lemma: "mammal"}}
	for _, d := range [][]Term{docA, docB, docC} {
		idf.Observe(d)
	}
	bm := NewBM25(idf)
	for _, d := range [][]Term{docA, docB, docC} {
		bm.Observe(d)
	}

	query := map[string]float64{"otter": 1.0}
	scoreA := bm.Score(docA, query)
	scoreB := bm.Score(docB, query)
	scoreC := bm.Score(docC, query)

	if scoreB != 0 {
		t.Errorf("a document with no query term scored %.3f, want 0", scoreB)
	}
	if !(scoreA > scoreC) {
		t.Errorf("two mentions (%.3f) should outscore one (%.3f)", scoreA, scoreC)
	}
	for name, s := range map[string]float64{"A": scoreA, "B": scoreB, "C": scoreC} {
		if s < 0 || s > 1 {
			t.Errorf("doc %s scored %.3f, outside [0,1] — the blend with semantics assumes normalisation", name, s)
		}
	}

	// Hand-computed check of the single-term case, so the formula itself is
	// pinned rather than only its ordering.
	//   docs=3, "otter" in 2 -> idf = ln(4/3)+1 = 1.287682
	//   avgLen = 3, docLen(A) = 3 -> the length factor is exactly 1
	//   tf=2 -> tfPart = 2*(1.2+1) / (2 + 1.2*1) = 4.4/3.2 = 1.375
	//   ceiling = 1*1.287682*(1.2+1) = 2.832900
	//   score = 1*1.287682*1.375 / 2.832900 = 0.625
	want := 0.625
	if math.Abs(scoreA-want) > 1e-6 {
		t.Errorf("BM25 on doc A = %.6f, want the hand-computed %.6f", scoreA, want)
	}

	if got := bm.Score(nil, query); got != 0 {
		t.Errorf("empty document scored %.3f, want 0", got)
	}
	if got := bm.Score(docA, nil); got != 0 {
		t.Errorf("empty query scored %.3f, want 0", got)
	}
}

// TestBM25CeilingIsReachable guards the flaw that made the lexical half of
// the score inert.
//
// An expanded query has hundreds of terms. Normalising against "all of them
// matching at saturation" produces a ceiling no document can approach: on a
// page genuinely about its topic the lexical score came out at 0.018, so the
// 30% of the blend meant to be carried by BM25 contributed under a hundredth
// and could not cover the named entities and jargon that semantics misses.
func TestBM25CeilingIsReachable(t *testing.T) {
	idf := NewIDF()
	// A document squarely on topic: it contains the query's core vocabulary.
	onTopic := []Term{
		{Lemma: "otter"}, {Lemma: "otter"}, {Lemma: "habitat"}, {Lemma: "habitat"},
		{Lemma: "kelp"}, {Lemma: "marine"}, {Lemma: "coastal"}, {Lemma: "mammal"},
	}
	offTopic := []Term{
		{Lemma: "accounting"}, {Lemma: "ledger"}, {Lemma: "tax"}, {Lemma: "audit"},
	}
	idf.Observe(onTopic)
	idf.Observe(offTopic)
	bm := NewBM25(idf)
	bm.Observe(onTopic)
	bm.Observe(offTopic)

	// A realistically expanded query: a few core terms at full weight and a
	// long tail of weak two-hop expansions, as Expand actually produces.
	query := map[string]float64{
		"otter": 1.0, "habitat": 1.0, "marine": 0.6, "coastal": 0.6, "kelp": 0.6,
		"mammal": 0.6,
	}
	for i := 0; i < 200; i++ {
		query[fmt.Sprintf("weak%d", i)] = 0.3
	}

	on := bm.Score(onTopic, query)
	off := bm.Score(offTopic, query)

	if on <= off {
		t.Errorf("on-topic %.4f must beat off-topic %.4f", on, off)
	}
	// The real assertion: a document matching the core vocabulary must score
	// like a match, not like a rounding error.
	if on < 0.15 {
		t.Errorf("an on-topic document scored %.4f; the ceiling is unreachable and "+
			"the lexical half of the blend is inert", on)
	}
	if on > 1 {
		t.Errorf("score %.4f exceeds 1; the blend assumes normalisation", on)
	}
	t.Logf("on-topic=%.4f off-topic=%.4f with %d query terms", on, off, len(query))
}

func TestBM25SaturatesOnRepetition(t *testing.T) {
	idf := NewIDF()
	bm := NewBM25(idf)
	base := []Term{{Lemma: "otter"}, {Lemma: "river"}}
	idf.Observe(base)
	bm.Observe(base)

	query := map[string]float64{"otter": 1}
	// Keyword stuffing must not beat a genuinely relevant page indefinitely.
	ten := make([]Term, 0, 10)
	hundred := make([]Term, 0, 100)
	for i := 0; i < 10; i++ {
		ten = append(ten, Term{Lemma: "otter"})
	}
	for i := 0; i < 100; i++ {
		hundred = append(hundred, Term{Lemma: "otter"})
	}
	tenScore, hundredScore := bm.Score(ten, query), bm.Score(hundred, query)
	if hundredScore-tenScore > 0.15 {
		t.Errorf("ten-fold repetition raised the score by %.3f; term frequency is not saturating",
			hundredScore-tenScore)
	}
}

func TestCombine(t *testing.T) {
	got := Combine(1, 0, 0.7, 1)
	if math.Abs(got.Total-0.7) > 1e-9 {
		t.Errorf("Total = %.3f, want 0.7", got.Total)
	}
	if got.Semantic != 1 || got.Lexical != 0 || got.Coverage != 1 {
		t.Errorf("the parts must survive the blend, got %+v", got)
	}
	if all := Combine(0.5, 0.5, 0.3, 1); math.Abs(all.Total-0.5) > 1e-9 {
		t.Errorf("equal halves should give the same total regardless of alpha, got %.3f", all.Total)
	}
	// An out-of-range alpha or coverage must not produce an out-of-range
	// score, which would break the frontier's threshold comparison.
	for _, bad := range []float64{-1, 2, math.NaN()} {
		if s := Combine(1, 1, bad, 1); s.Total < 0 || s.Total > 1 {
			t.Errorf("alpha %v produced total %.3f, outside [0,1]", bad, s.Total)
		}
		if s := Combine(1, 1, 0.7, bad); s.Total < 0 || s.Total > 1 {
			t.Errorf("coverage %v produced total %.3f, outside [0,1]", bad, s.Total)
		}
	}
}

// TestCombineFallsBackToLexicalWhenUnmeasurable: a query of pure domain
// jargon has no semantic score to speak of. Weighting a meaningless 0 at
// alpha would push every page down by the same amount and make the threshold
// meaningless, so the blend hands that weight to BM25 instead.
func TestCombineScalesWithCoverage(t *testing.T) {
	const semantic, lexical, alpha = 0.0, 0.9, 0.7

	full := Combine(semantic, lexical, alpha, 1)
	none := Combine(semantic, lexical, alpha, 0)
	half := Combine(semantic, lexical, alpha, 0.5)

	// Nothing measurable -> the lexical score stands on its own.
	if math.Abs(none.Total-lexical) > 1e-9 {
		t.Errorf("coverage 0 gave total %.3f, want the lexical score %.3f", none.Total, lexical)
	}
	// Fully measurable -> the configured blend, and a 0 semantic really does
	// count against the page.
	if math.Abs(full.Total-0.3*lexical) > 1e-9 {
		t.Errorf("coverage 1 gave total %.3f, want %.3f", full.Total, 0.3*lexical)
	}
	if !(none.Total > half.Total && half.Total > full.Total) {
		t.Errorf("total should rise as coverage falls: none=%.3f half=%.3f full=%.3f",
			none.Total, half.Total, full.Total)
	}
}

// TestQueryTermsKeepsWordsTheInventoryDoesNotKnow is the lexical half's
// guarantee that it can cover for the semantic half.
//
// Building the BM25 vocabulary from the expansion alone silently dropped every
// query word without a sense — which is to say product names, jargon and
// acronyms, the most discriminating words a technical query has. Measured on a
// real crawl, "opensearch" was absent from the term set entirely, the lexical
// score collapsed to noise, and a flat semantic score ranked the crawl into
// the site's help pages.
func TestQueryTermsKeepsWordsTheInventoryDoesNotKnow(t *testing.T) {
	terms := []Term{
		{Surface: "opensearch", Lemma: "opensearch", POS: POSNoun}, // no sense anywhere
		{Surface: "indices", Lemma: "index", POS: POSNoun},         // resolved below
	}
	expansion := []WeightedSense{
		{SynsetID: "n06491786", Lemmas: []string{"index"}, Relation: "synonym", Weight: 1},
		{SynsetID: "n1234567", Lemmas: []string{"listing"}, Relation: RelationHypernym, Weight: 0.6},
	}

	// The old behaviour, kept as the contrast that makes the point.
	if _, ok := ExpandedTerms(expansion)["opensearch"]; ok {
		t.Fatal("fixture is wrong: the expansion must not already contain the unknown word")
	}

	got := QueryTerms(terms, expansion, 0)
	if weight := got["opensearch"]; weight != 1 {
		t.Errorf("opensearch weight = %v, want 1 — a crawl for a product must be able "+
			"to match the product's name", weight)
	}
	// The expansion still contributes everything it did before.
	if weight := got["listing"]; weight != 0.6 {
		t.Errorf("listing weight = %v, want the expansion's 0.6", weight)
	}
	// A query word that IS resolved keeps the higher of the two, not the lower.
	if weight := got["index"]; weight != 1 {
		t.Errorf("index weight = %v, want 1", weight)
	}
}

// TestQueryTermsNeverLowersAnExpansionWeight guards the merge direction: the
// query's words are a floor, not an override.
func TestQueryTermsNeverLowersAnExpansionWeight(t *testing.T) {
	terms := []Term{{Surface: "otters", Lemma: "otter", POS: POSNoun}}
	expansion := []WeightedSense{
		{SynsetID: "s:otter", Lemmas: []string{"otter"}, Relation: "synonym", Weight: 1},
	}
	if weight := QueryTerms(terms, expansion, 0)["otter"]; weight != 1 {
		t.Errorf("otter weight = %v, want the expansion's 1 to survive", weight)
	}
}

// TestQueryTermsWeighsNamesAboveOrdinaryWords is the fix for the case that
// prompted it: "opensearch database documentation, syntax and query language"
// returned a Postgres page, because a Postgres page matches five of the six
// words and misses only the one that decides anything.
func TestQueryTermsWeighsNamesAboveOrdinaryWords(t *testing.T) {
	terms := []Term{
		{Surface: "opensearch", Lemma: "opensearch", POS: POSNoun, Entity: true},
		{Surface: "database", Lemma: "database", POS: POSNoun},
		{Surface: "documentation", Lemma: "documentation", POS: POSNoun},
		{Surface: "syntax", Lemma: "syntax", POS: POSNoun},
		{Surface: "query", Lemma: "query", POS: POSNoun},
		{Surface: "language", Lemma: "language", POS: POSNoun},
	}
	got := QueryTerms(terms, nil, 0)

	if got["opensearch"] <= got["database"] {
		t.Fatalf("opensearch %v does not outweigh database %v", got["opensearch"], got["database"])
	}
	if got["opensearch"] != DefaultEntityWeight {
		t.Errorf("opensearch weight = %v, want the default entity weight %v",
			got["opensearch"], DefaultEntityWeight)
	}
	for _, ordinary := range []string{"database", "documentation", "syntax", "query", "language"} {
		if got[ordinary] != 1 {
			t.Errorf("%s weight = %v, want 1", ordinary, got[ordinary])
		}
	}

	// The property that decides the crawl: a page matching every ordinary word
	// but not the name must fall short of one that matches the name too.
	var withoutName, withName float64
	for lemma, weight := range got {
		if lemma != "opensearch" {
			withoutName += weight
		}
		withName += weight
	}
	if withoutName/withName > 0.7 {
		t.Errorf("a page missing only the name still reaches %.0f%% of the query's weight; "+
			"that is not enough separation to keep the wrong product out",
			100*withoutName/withName)
	}
}

// TestAnalyzeMarksNamesBySpelling covers what the text can assert on its own,
// now that absence from the dictionary is no longer taken as evidence.
func TestAnalyzeMarksNamesBySpelling(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("we deployed OpenSearch and gRPC over IPv6 on Amazon, using an ORM and a database")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, term := range terms {
		got[term.Lemma] = term.Entity
	}
	// A capital inside the word, letters mixed with digits, an all-capital
	// run, and a capitalised proper noun: spellings the industry uses BECAUSE
	// they mark a name.
	for _, name := range []string{"opensearch", "grpc", "ipv6", "amazon", "orm"} {
		if !got[name] {
			t.Errorf("%q was not marked as a name", name)
		}
	}
	for _, common := range []string{"database", "deploy"} {
		if got[common] {
			t.Errorf("%q was marked as a name; it is an ordinary word", common)
		}
	}
}

// TestAnalyzeDoesNotGuessFromTheDictionary pins the removal itself.
//
// A lowercase mention of a name looks exactly like any other lowercase word,
// and the crawler no longer pretends otherwise. Absence from WordNet used to
// stand in for "is a name" and it was a bad proxy — it also marked
// `rebalancing`, and every typo a user made, then gave them triple weight.
// The gap this leaves is closed by CollectNames, from evidence rather than
// from a guess.
func TestAnalyzeDoesNotGuessFromTheDictionary(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("documetation for the databse and rebalancing")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range terms {
		if term.Entity {
			t.Errorf("%q was marked a name; nothing in the text says it is one, "+
				"it is merely absent from the dictionary", term.Lemma)
		}
	}
}

// TestCollectNamesTeachesALowercaseQuery is the replacement working: the
// pages a crawl was pointed at say which of the query's words are names.
func TestCollectNamesTeachesALowercaseQuery(t *testing.T) {
	tax := testTaxonomy(t)
	// How a person types it.
	query, err := tax.Analyze("opensearch database documentation")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range query {
		if term.Entity {
			t.Fatalf("%q is already marked; the fixture must start with nothing known", term.Lemma)
		}
	}
	// How the seed page writes it.
	page, err := tax.Analyze("Teams running OpenSearch in production often compare it to Elasticsearch.")
	if err != nil {
		t.Fatal(err)
	}
	names := CollectNames(page)
	if !names["opensearch"] {
		t.Fatalf("the page did not assert opensearch as a name; got %v", names)
	}
	MarkNames(query, names)

	var marked []string
	for _, term := range query {
		if term.Entity {
			marked = append(marked, term.Lemma)
		}
	}
	if len(marked) != 1 || marked[0] != "opensearch" {
		t.Errorf("marked %v, want exactly [opensearch] — the ordinary words must not be swept up", marked)
	}
}

// TestAnalyzeDoesNotTreatTheFirstWordAsAName guards the obvious false
// positive: a capital letter at the start of a text means a sentence began,
// not that something was named.
func TestAnalyzeDoesNotTreatTheFirstWordAsAName(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("Database documentation and query syntax")
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) == 0 {
		t.Fatal("no terms")
	}
	if terms[0].Lemma == "database" && terms[0].Entity {
		t.Error("the leading capitalised word was treated as a name")
	}
}

// TestAnalyzeKeepsWordsTheTaggerDiscards guards the token-rescue path, which
// is a separate decision from naming and must not be confused with it: it is
// about whether a word reaches scoring at all.
//
// Measured, prose tagged `opensearch` an ADVERB in "opensearch shard
// allocation tuning" — adverbs are dropped — and the one word the query was
// about vanished before scoring saw it. A tagger has no evidence for a word it
// has never seen, so an out-of-vocabulary token is kept as a noun.
func TestAnalyzeKeepsWordsTheTaggerDiscards(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("opensearch shard allocation tuning")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, term := range terms {
		if term.Lemma == "opensearch" {
			found = true
		}
	}
	if !found {
		t.Errorf("opensearch did not survive tagging; got %v", lemmas(terms))
	}
	// Function words must still go, or the rescue swallows the whole language.
	dropped, err := tax.Analyze("the quick brown fox jumped over their lazy dog")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range dropped {
		if term.Lemma == "the" || term.Lemma == "their" {
			t.Errorf("%q was rescued; it is a function word", term.Lemma)
		}
	}
}

// TestAnalyzeKeepsMultiWordNamesWhole is what entity extraction is turned on
// for, and the only thing it does that the cheaper rules cannot.
//
// Without it "Amazon OpenSearch Service" is three unrelated words, and a page
// about Amazon Web Services matches a query about it exactly as well as a page
// about the thing being asked for. The span is emitted ALONGSIDE its parts, so
// the words still count individually and the name additionally counts once as
// itself — on both sides, because query and page run through the same code.
func TestAnalyzeKeepsMultiWordNamesWhole(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("running Amazon OpenSearch Service in production")
	if err != nil {
		t.Fatal(err)
	}
	var span *Term
	parts := map[string]bool{}
	for i := range terms {
		if terms[i].Lemma == "amazon opensearch service" {
			span = &terms[i]
		}
		parts[terms[i].Lemma] = true
	}
	if span == nil {
		t.Fatalf("the multi-word name was not kept whole; got %v", lemmas(terms))
	}
	if !span.Entity {
		t.Error("the span was not marked as a name")
	}
	// The parts must survive too, or a page saying only "OpenSearch" stops
	// matching at all.
	for _, part := range []string{"amazon", "opensearch", "service"} {
		if !parts[part] {
			t.Errorf("%q was lost when the span was formed; got %v", part, lemmas(terms))
		}
	}
	// A single-word entity must NOT be duplicated as a span, or its frequency
	// doubles in every count that matters.
	single, err := tax.Analyze("deploying Kubernetes in production")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, term := range single {
		if term.Lemma == "kubernetes" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("kubernetes appears %d times, want 1; single-word names must not be doubled", seen)
	}
}

func lemmas(terms []Term) []string {
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		out = append(out, term.Lemma)
	}
	return out
}

// TestUsableSpanRejectsExtractorDrift keeps entity extraction from turning
// page furniture into query vocabulary.
//
// Measured on a real crawled page, prose produced "data is damning", "hands
// fire jump" and a seventeen-word run of navigation starting "dev organization
// accounts dev showcase about contact". Every one of those was becoming a term
// of the page: counted in its length, matchable, eligible for the salience
// budget. Newswire-trained extraction drifts badly over chrome and broken
// sentences, which is most of what a crawler fetches.
func TestUsableSpanRejectsExtractorDrift(t *testing.T) {
	cases := []struct {
		what  string
		parts []string
		want  string
	}{
		{"a real product name", []string{"Amazon", "OpenSearch", "Service"}, "amazon opensearch service"},
		{"a two-word name", []string{"Model", "Context"}, "model context"},
		{"punctuation is trimmed", []string{"Apache", "Kafka,"}, "apache kafka"},

		{"a single word is not a span", []string{"Kubernetes"}, ""},
		{"longer than a name ever is", []string{"dev", "organization", "accounts", "dev", "showcase"}, ""},
		{"contains a function word", []string{"data", "is", "damning"}, ""},
		{"contains a preposition", []string{"hands", "on", "fire"}, ""},
		{"nothing left after trimming", []string{"a", "-"}, ""},
	}
	for _, tc := range cases {
		got, ok := usableSpan(tc.parts)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: %v was kept as %q, want it rejected", tc.what, tc.parts, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("%s: %v -> (%q, %v), want %q", tc.what, tc.parts, got, ok, tc.want)
		}
	}
}
