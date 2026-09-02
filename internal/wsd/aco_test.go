package wsd

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// technicalInventory is the failure that motivated the colony, reduced to
// three words.
//
// Each word's FIRST sense is its everyday one and shares nothing with the
// query's other words, so greedy Lesk — which scores each word against the
// surrounding text and falls back to the first sense when nothing overlaps —
// takes all three everyday senses. The three technical senses share a great
// deal with EACH OTHER and nothing with the context, so only an algorithm that
// scores an assignment as a whole can find them.
//
// This mirrors the real case: "opensearch indices, security, syntax and usage"
// was read as software, semiotics, collateral and habit.
func technicalInventory() *fakeInventory {
	syn := func(id, lemma, gloss string) Synset {
		return Synset{ID: id, POS: POSNoun, Lemmas: []string{lemma}, Gloss: gloss}
	}
	senses := map[string][]Synset{
		POSNoun + ":index": {
			syn("s:index-sign", "index", "a philosophical theory of the functions of signs and symbols"),
			syn("s:index-db", "index", "a structure that improves the speed of retrieval in a database system"),
		},
		POSNoun + ":security": {
			syn("s:security-collateral", "security", "property that a creditor can claim upon default of an obligation"),
			syn("s:security-computer", "security", "protection of a database system against unauthorised retrieval"),
		},
		POSNoun + ":cluster": {
			syn("s:cluster-bunch", "cluster", "a grouping of a number of similar things growing together"),
			syn("s:cluster-computing", "cluster", "connected nodes serving one database system with shared retrieval"),
		},
	}
	synsets := map[string]Synset{}
	for _, list := range senses {
		for _, s := range list {
			synsets[s.ID] = s
		}
	}
	return &fakeInventory{senses: senses, synsets: synsets}
}

func technicalTerms() []Term {
	return []Term{
		{Surface: "index", Lemma: "index", POS: POSNoun},
		{Surface: "security", Lemma: "security", POS: POSNoun},
		{Surface: "cluster", Lemma: "cluster", POS: POSNoun},
	}
}

func chosen(senses []Sense) map[string]string {
	out := map[string]string{}
	for _, s := range senses {
		out[s.Term.Lemma] = s.SynsetID
	}
	return out
}

// TestGreedyLeskPicksIncoherentSenses is the control. It is not testing a bug
// to be fixed — greedy Lesk is doing exactly what it was designed to do — but
// the colony's whole justification is that this outcome is possible, so it is
// worth pinning down. If a change ever makes greedy Lesk solve this, the
// colony's cost needs re-examining.
func TestGreedyLeskPicksIncoherentSenses(t *testing.T) {
	inv := technicalInventory()
	terms := technicalTerms()

	senses, err := Disambiguate(context.Background(), inv, nil, terms, LeskOptions{MaxSenses: 4})
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	got := chosen(senses)
	for lemma, want := range map[string]string{
		"index": "s:index-sign", "security": "s:security-collateral", "cluster": "s:cluster-bunch",
	} {
		if got[lemma] != want {
			t.Errorf("greedy chose %s for %q, want %s — the premise of the colony has changed",
				got[lemma], lemma, want)
		}
	}
}

// TestACOFindsTheCoherentReading is the point of the whole file.
func TestACOFindsTheCoherentReading(t *testing.T) {
	inv := technicalInventory()
	terms := technicalTerms()

	// The context is the query's own lemmas and nothing else, which is the
	// hard case: no external evidence, so the only thing that can distinguish
	// the readings is whether the senses agree with one another.
	senses, err := DisambiguateACO(context.Background(), inv, nil, terms,
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}})
	if err != nil {
		t.Fatalf("DisambiguateACO: %v", err)
	}
	got := chosen(senses)
	for lemma, want := range map[string]string{
		"index": "s:index-db", "security": "s:security-computer", "cluster": "s:cluster-computing",
	} {
		if got[lemma] != want {
			t.Errorf("colony chose %s for %q, want %s", got[lemma], lemma, want)
		}
	}
}

// TestACOCostsNoExtraLookups is the property that makes the colony affordable
// on a metered dictionary: it thinks harder about the same evidence, it does
// not gather more. If this ever regresses, the BabelNet bill regresses with it.
func TestACOCostsNoExtraLookups(t *testing.T) {
	terms := technicalTerms()
	ctxBag := []string{"index", "security", "cluster"}

	greedyInv := technicalInventory()
	if _, err := Disambiguate(context.Background(), greedyInv, nil, terms, LeskOptions{MaxSenses: 4}); err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	acoInv := technicalInventory()
	if _, err := DisambiguateACO(context.Background(), acoInv, nil, terms, ctxBag,
		ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}); err != nil {
		t.Fatalf("DisambiguateACO: %v", err)
	}
	if acoInv.calls != greedyInv.calls {
		t.Errorf("colony made %d inventory calls, greedy made %d; they must be identical",
			acoInv.calls, greedyInv.calls)
	}
}

// TestACOIsDeterministic protects a user-facing promise: two runs of the same
// crawl produce the same report, so a difference between two reports means the
// corpus changed rather than the dice.
func TestACOIsDeterministic(t *testing.T) {
	terms := technicalTerms()
	ctxBag := []string{"index", "security", "cluster"}

	var first map[string]string
	for i := 0; i < 3; i++ {
		senses, err := DisambiguateACO(context.Background(), technicalInventory(), nil, terms, ctxBag,
			ACOOptions{Lesk: LeskOptions{MaxSenses: 4}})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := chosen(senses)
		if first == nil {
			first = got
			continue
		}
		for lemma, id := range first {
			if got[lemma] != id {
				t.Errorf("run %d chose %s for %q, run 0 chose %s", i, got[lemma], lemma, id)
			}
		}
	}
}

// TestACOUsesTheSurroundingContext is the seed-page mechanism in miniature:
// the same word, the same inventory, two different bodies of surrounding text,
// two different readings.
func TestACOUsesTheSurroundingContext(t *testing.T) {
	terms := []Term{{Surface: "bank", Lemma: "bank", POS: POSNoun}}
	cases := []struct {
		what    string
		context []string
		want    string
	}{
		{"a money context", []string{"deposit", "money", "lending", "financial"}, "s:bank-money"},
		{"a river context", []string{"water", "sloping", "land", "river"}, "s:bank-river"},
	}
	for _, tc := range cases {
		senses, err := DisambiguateACO(context.Background(), bankInventory(), nil, terms, tc.context,
			ACOOptions{Lesk: LeskOptions{MaxSenses: 4, MaxRelated: 4}})
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if got := senses[0].SynsetID; got != tc.want {
			t.Errorf("%s: chose %s, want %s", tc.what, got, tc.want)
		}
	}
}

// TestACOReturnsWhatItResolvedWhenExhausted keeps the colony usable on a
// metered inventory: a query half-read is still worth crawling towards, and
// the caller needs the error as well as the senses.
func TestACOReturnsWhatItResolvedWhenExhausted(t *testing.T) {
	inv := &budgetedInventory{
		fakeInventory: technicalInventory(),
		// Two words resolve, the third does not. These senses have no
		// relations, so one call per word is the whole cost.
		limit: 2,
		err:   fmt.Errorf("test: %w", ErrInventoryExhausted),
	}
	senses, err := DisambiguateACO(context.Background(), inv, nil, technicalTerms(),
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}})
	if err == nil {
		t.Fatal("err = nil, want the exhaustion reported")
	}
	if len(senses) != 3 {
		t.Fatalf("got %d senses, want one per term even when the inventory ran out", len(senses))
	}
	if senses[0].SynsetID == "" {
		t.Error("the first term resolved before the allowance ran out; it must still be reported")
	}
}

// budgetedInventory refuses after a fixed number of calls.
type budgetedInventory struct {
	*fakeInventory
	limit int
	err   error
}

func (b *budgetedInventory) Senses(ctx context.Context, lemma, pos string) ([]Synset, error) {
	if b.calls >= b.limit {
		return nil, b.err
	}
	return b.fakeInventory.Senses(ctx, lemma, pos)
}

func (b *budgetedInventory) Synset(ctx context.Context, id string) (Synset, error) {
	if b.calls >= b.limit {
		return Synset{}, b.err
	}
	return b.fakeInventory.Synset(ctx, id)
}

// The three defects below were found by running the disambiguation in
// isolation against a real query and brute-forcing the objective. The colony
// was finding the exact optimum every time; what it was maximising was wrong.

// TestOverlapIgnoresFunctionWords pins the largest of the three. Two glosses
// sharing nothing but pronouns and modals were scoring 17 — enough to make the
// collateral sense of "security" and the act-of-using sense of "usage" look
// like the most coherent reading of a query about software.
func TestOverlapIgnoresFunctionWords(t *testing.T) {
	a := tokenizeGloss("he can use it when they have one of these into which you would")
	b := tokenizeGloss("you can have one when he might do this into that which they would")
	if got := overlap(a, b); got != 0 {
		t.Errorf("overlap = %v on function words alone, want 0 (tokens a=%v)", got, a)
	}
	// The positive control: real shared vocabulary must still score, and the
	// contiguous-run squaring must survive.
	c := tokenizeGloss("a sloping body of water beside the river")
	d := tokenizeGloss("the body of water that borders the land")
	if got := overlap(c, d); got < 4 {
		t.Errorf("overlap = %v on a shared phrase, want at least 4 for a two-word run", got)
	}
}

// TestOverlapIgnoresExampleSentences covers the second. WordNet glosses carry
// illustrative sentences, which are prose about arbitrary subjects; two senses
// were matching on "united states" lifted out of two unrelated examples.
func TestOverlapIgnoresExampleSentences(t *testing.T) {
	withExample := `a formal declaration of fact; "issued by the united states treasury"`
	plain := `a habitual practice; "common across the united states"`
	if got := overlap(tokenizeGloss(withExample), tokenizeGloss(plain)); got > 0 {
		t.Errorf("overlap = %v on example sentences alone, want 0", got)
	}
	// The definitions themselves must still be compared.
	if got := overlap(
		tokenizeGloss(`a formal declaration of fact; "one example"`),
		tokenizeGloss(`a declaration of fact; "another example"`)); got < 4 {
		t.Errorf("overlap = %v between the definitions, want the shared phrase to count", got)
	}
}

// TestRelatednessIsNotWonByBagSize covers the third, and the subtlest. Raw
// overlap is an absolute count, so a sense with many hyponyms accumulates a
// larger extended gloss and out-scores everything whatever it means: measured,
// the winning sense of "security" carried 135 tokens against the right one's
// 18.
func TestRelatednessIsNotWonByBagSize(t *testing.T) {
	query := strings.Fields("encrypted network traffic protection")

	// Short and almost entirely about the query.
	precise := strings.Fields("protection encrypted network")
	// Longer, and it matches MORE of the query in absolute terms — but the
	// match is a small fraction of what it is about. This is the shape of the
	// real case: the sense that won had every financial instrument in WordNet
	// hanging off it, and won on sheer surface area.
	vague := append(strings.Fields("encrypted network traffic"),
		strings.Fields("finance investment stocks bonds property lending banking "+
			"insurance mortgage deposit interest dividend equity certificate "+
			"treasury exchange broker portfolio asset liability revenue capital "+
			"collateral obligation creditor debtor principal maturity coupon")...)

	// The premise: raw overlap really does prefer the vague one.
	if overlap(vague, query) <= overlap(precise, query) {
		t.Fatalf("fixture is wrong: raw overlap vague=%v precise=%v, the vague bag must win on the raw count",
			overlap(vague, query), overlap(precise, query))
	}
	// The fix: as a share of what each sense is about, the precise one wins.
	if relatedness(precise, query) <= relatedness(vague, query) {
		t.Errorf("relatedness precise=%.3f vague=%.3f; normalising by bag size is not working",
			relatedness(precise, query), relatedness(vague, query))
	}
}
