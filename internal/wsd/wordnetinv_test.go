package wsd

import (
	"context"
	"testing"
)

func TestWordNetInventorySensesAreFrequencyOrdered(t *testing.T) {
	tax := testTaxonomy(t)
	inv := NewWordNetInventory(tax)
	ctx := context.Background()

	senses, err := inv.Senses(ctx, "cat", POSNoun)
	if err != nil {
		t.Fatalf("Senses: %v", err)
	}
	if len(senses) < 2 {
		t.Fatalf("got %d senses of 'cat', want several", len(senses))
	}
	// The first sense must be the animal, not the CAT scan. Unranked order
	// puts the scan first, which is what made wup(dog,cat) collapse from
	// 0.857 to 0.100 when this was first measured.
	first := senses[0]
	if first.WordNetKey != "n02121620" {
		t.Errorf("first sense of 'cat' is %q (%v), want the true cat n02121620",
			first.WordNetKey, first.Lemmas)
	}
	if first.Gloss == "" {
		t.Error("a sense arrived without a gloss; Lesk has nothing to compare")
	}
	// Senses must be distinct.
	seen := map[string]bool{}
	for _, s := range senses {
		if seen[s.ID] {
			t.Errorf("duplicate sense %q in the candidate list", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestWordNetInventoryRelations(t *testing.T) {
	tax := testTaxonomy(t)
	inv := NewWordNetInventory(tax)

	dog, err := inv.Synset(context.Background(), "n02084071")
	if err != nil {
		t.Fatalf("Synset: %v", err)
	}
	if dog.ID != "n02084071" || dog.WordNetKey != "n02084071" {
		t.Errorf("id/key = %q/%q, want both n02084071", dog.ID, dog.WordNetKey)
	}
	if dog.POS != POSNoun {
		t.Errorf("POS = %q, want %q", dog.POS, POSNoun)
	}
	kinds := map[string]int{}
	for _, rel := range dog.Relations {
		kinds[rel.Type]++
	}
	// A dog has a hypernym (canine), hyponyms (breeds) and a holonym
	// (Canis / pack). Getting the '#' vs '%' inversion wrong would swap the
	// last two.
	for _, want := range []string{RelationHypernym, RelationHyponym, RelationHolonym} {
		if kinds[want] == 0 {
			t.Errorf("no %s relations on dog; got %v", want, kinds)
		}
	}
}

func TestWordNetInventoryUnknownIsNotAnError(t *testing.T) {
	tax := testTaxonomy(t)
	inv := NewWordNetInventory(tax)
	ctx := context.Background()

	got, err := inv.Synset(ctx, "n99999999")
	if err != nil {
		t.Errorf("unknown id returned an error: %v", err)
	}
	if got.ID != "" {
		t.Errorf("unknown id resolved to %q, want the zero synset", got.ID)
	}
	senses, err := inv.Senses(ctx, "zzzznotaword", POSNoun)
	if err != nil || len(senses) != 0 {
		t.Errorf("unknown lemma = (%v, %v), want (empty, nil)", senses, err)
	}
}

func TestPointerRelation(t *testing.T) {
	cases := map[string]string{
		"@": RelationHypernym,
		"~": RelationHyponym,
		// Instance relations are NOT hypernyms/hyponyms: "Aegean Sea" is an
		// instance of sea, not a kind of it. Expansion refuses to follow
		// them, which is what keeps proper nouns out of the query.
		"@i": RelationInstance, "~i": RelationInstance,
		"%m": RelationMeronym, "%s": RelationMeronym, "%p": RelationMeronym,
		"#m": RelationHolonym, "#s": RelationHolonym, "#p": RelationHolonym,
		"+": RelationDerivation,
		"!": RelationOther, "=": RelationOther, "": RelationOther,
	}
	for symbol, want := range cases {
		if got := pointerRelation(symbol); got != want {
			t.Errorf("pointerRelation(%q) = %q, want %q", symbol, got, want)
		}
	}
}

// TestWordNetDisambiguatesAPage is the page-side path end to end: local
// senses, local Lesk, no network and no budget.
func TestWordNetDisambiguatesAPage(t *testing.T) {
	tax := testTaxonomy(t)
	inv := NewWordNetInventory(tax)

	terms, err := tax.Analyze(
		"The river bank was eroding where the water met the sloping land.")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	senses, err := Disambiguate(context.Background(), inv, tax, terms, DefaultLeskOptions)
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	var bank Sense
	for _, s := range senses {
		if s.Term.Lemma == "bank" {
			bank = s
			break
		}
	}
	if bank.SynsetID == "" {
		t.Fatalf("'bank' was not resolved; senses: %v", senses)
	}
	// The geographic context must not yield the financial institution.
	const financialInstitution = "n08420278"
	if bank.WordNetKey == financialInstitution {
		t.Errorf("'bank' in a river context resolved to the financial sense %q", bank.WordNetKey)
	}
	t.Logf("bank -> %s (%.1f): %.60s", bank.WordNetKey, bank.Score, bank.Gloss)
}

// TestExpansionStaysTopical guards against the drift found by running a real
// query through the pipeline: "sea otter habitat conservation along the river
// bank" expanded to 634 senses and 1268 BM25 terms, among them Acheronian,
// Tar Heel State, Portugal and Illinois River.
//
// Those come from instance relations — every named river is an instance of
// "river" — and from unbounded fan-out on broad nouns. Both are now bounded,
// and a query about otters must not produce a gazetteer.
func TestExpansionStaysTopical(t *testing.T) {
	tax := testTaxonomy(t)
	inv := NewWordNetInventory(tax)
	ctx := context.Background()

	terms, err := tax.Analyze("sea otter habitat conservation along the river bank")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	senses, err := Disambiguate(ctx, inv, tax, terms, DefaultLeskOptions)
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	expansion, err := Expand(ctx, inv, senses, DefaultExpandOptions)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	if len(expansion) > DefaultMaxExpansion {
		t.Errorf("expansion has %d senses, above the %d cap", len(expansion), DefaultMaxExpansion)
	}
	for _, ws := range expansion {
		if ws.Relation == RelationInstance {
			t.Errorf("expansion followed an instance relation to %s (%v)", ws.SynsetID, ws.Lemmas)
		}
	}
	// The proper nouns that made the original output unusable.
	lemmas := ExpandedTerms(expansion)
	for _, unwanted := range []string{
		"portugal", "aegean sea", "illinois river", "tar heel state", "mediterranean sea",
	} {
		if _, found := lemmas[unwanted]; found {
			t.Errorf("%q is in the expanded query; instances are leaking back in", unwanted)
		}
	}
	// It must still expand usefully — the cap is not allowed to be a mute.
	if len(expansion) < 20 {
		t.Errorf("expansion collapsed to %d senses; the bounds are too tight to be useful", len(expansion))
	}
	t.Logf("expansion: %d senses, %d BM25 lemmas", len(expansion), len(lemmas))
}

// TestCrossInventoryConceptsAreComparable is the property the whole split
// design rests on: a query concept from BabelNet and a page concept from
// WordNet must be measurable against each other.
func TestCrossInventoryConceptsAreComparable(t *testing.T) {
	tax := testTaxonomy(t)

	// What the BabelNet inventory produces for the query: its own id, with
	// the WordNet offset carried alongside.
	queryConcept := Concept{
		SynsetID: "bn:00015267n", WordNetKey: NormalizeWordNetKey("wn:02084071n"), Weight: 1,
	}
	// What the WordNet inventory produces for a page: the key as the id.
	pageSame := Concept{SynsetID: "n02084071", WordNetKey: "n02084071", Weight: 1}
	pageRelated := Concept{SynsetID: "n02121620", WordNetKey: "n02121620", Weight: 1}
	pageUnrelated := Concept{SynsetID: "n09213565", WordNetKey: "n09213565", Weight: 1}

	same := Similarity(tax, []Concept{queryConcept}, []Concept{pageSame})
	if same != 1 {
		t.Errorf("the same concept from two inventories scored %.3f, want 1 — "+
			"the ids differ, so this can only work through the WordNet key", same)
	}
	related := Similarity(tax, []Concept{queryConcept}, []Concept{pageRelated})
	unrelated := Similarity(tax, []Concept{queryConcept}, []Concept{pageUnrelated})
	if !(same > related && related > unrelated) {
		t.Errorf("ordering broken: same=%.3f related=%.3f unrelated=%.3f", same, related, unrelated)
	}

	// A BabelNet-only sense (no WordNet counterpart) cannot match
	// semantically. That is expected, not a bug — BM25 covers it — but it
	// must be 0 rather than an accident.
	entity := Concept{SynsetID: "bn:03083790n", WordNetKey: "", Weight: 1}
	if got := Similarity(tax, []Concept{entity}, []Concept{pageSame}); got != 0 {
		t.Errorf("a BabelNet-only concept scored %.3f against a WordNet one, want 0", got)
	}
}
