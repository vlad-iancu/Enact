package wsd

import (
	"context"
	"testing"
)

// fakeInventory is a hand-built sense inventory. Every test in this package
// runs against it rather than BabelNet: the algorithms are what is under
// test, and a metered network inventory would make the suite slow, flaky and
// expensive.
type fakeInventory struct {
	senses  map[string][]Synset // keyed pos + ":" + lemma
	synsets map[string]Synset
	calls   int
}

func (f *fakeInventory) Senses(_ context.Context, lemma, pos string) ([]Synset, error) {
	f.calls++
	return f.senses[pos+":"+lemma], nil
}

func (f *fakeInventory) Synset(_ context.Context, id string) (Synset, error) {
	f.calls++
	return f.synsets[id], nil
}

// bankInventory is the canonical WSD example: "bank" as a financial
// institution versus "bank" as the side of a river.
func bankInventory() *fakeInventory {
	financial := Synset{
		ID: "s:bank-money", POS: POSNoun, Lemmas: []string{"bank"},
		Gloss:     "a financial institution that accepts deposits and channels the money into lending activities",
		Relations: []Relation{{Target: "s:institution", Type: RelationHypernym}},
	}
	river := Synset{
		ID: "s:bank-river", POS: POSNoun, Lemmas: []string{"bank"},
		Gloss:     "sloping land beside a body of water",
		Relations: []Relation{{Target: "s:slope", Type: RelationHypernym}},
	}
	return &fakeInventory{
		senses: map[string][]Synset{
			POSNoun + ":bank":     {financial, river},
			POSNoun + ":money":    {{ID: "s:money", POS: POSNoun, Gloss: "the most common medium of exchange; funds deposits lending"}},
			POSNoun + ":deposit":  {{ID: "s:deposit", POS: POSNoun, Gloss: "money deposited in a bank account"}},
			POSNoun + ":river":    {{ID: "s:river", POS: POSNoun, Gloss: "a large natural stream of water flowing over sloping land"}},
			POSNoun + ":water":    {{ID: "s:water", POS: POSNoun, Gloss: "a body of water covering sloping land"}},
			POSNoun + ":mortgage": {{ID: "s:mortgage", POS: POSNoun, Gloss: "a lending instrument secured against property"}},
		},
		synsets: map[string]Synset{
			"s:institution": {ID: "s:institution", POS: POSNoun, Gloss: "an organization founded for financial or social purpose"},
			"s:slope":       {ID: "s:slope", POS: POSNoun, Gloss: "an elevated geological formation beside water"},
		},
	}
}

func TestDisambiguatePicksSenseFromContext(t *testing.T) {
	inv := bankInventory()
	cases := []struct {
		name  string
		terms []Term
		want  string
	}{
		{
			name: "financial context",
			terms: []Term{
				{Surface: "bank", Lemma: "bank", POS: POSNoun},
				{Surface: "money", Lemma: "money", POS: POSNoun},
				{Surface: "deposits", Lemma: "deposit", POS: POSNoun},
				{Surface: "mortgage", Lemma: "mortgage", POS: POSNoun},
			},
			want: "s:bank-money",
		},
		{
			name: "geographic context",
			terms: []Term{
				{Surface: "bank", Lemma: "bank", POS: POSNoun},
				{Surface: "river", Lemma: "river", POS: POSNoun},
				{Surface: "water", Lemma: "water", POS: POSNoun},
			},
			want: "s:bank-river",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			senses, err := Disambiguate(context.Background(), inv, nil, tc.terms, DefaultLeskOptions)
			if err != nil {
				t.Fatalf("Disambiguate: %v", err)
			}
			if len(senses) != len(tc.terms) {
				t.Fatalf("got %d senses, want %d", len(senses), len(tc.terms))
			}
			if got := senses[0].SynsetID; got != tc.want {
				t.Errorf("bank resolved to %q, want %q (score %.1f)", got, tc.want, senses[0].Score)
			}
			if len(senses[0].Candidates) != 2 {
				t.Errorf("recorded %d candidates, want both senses on the record", len(senses[0].Candidates))
			}
		})
	}
}

// TestExtendedLeskUsesRelatedGlosses is the test that proves the "extended"
// half is doing work: the overlap that decides this case exists ONLY in a
// hypernym's gloss, so plain Lesk (MaxRelated 0) cannot see it.
func TestExtendedLeskUsesRelatedGlosses(t *testing.T) {
	target := Synset{
		ID: "s:target", POS: POSNoun, Lemmas: []string{"widget"},
		Gloss:     "an unremarkable thing",
		Relations: []Relation{{Target: "s:parent", Type: RelationHypernym}},
	}
	decoy := Synset{
		ID: "s:decoy", POS: POSNoun, Lemmas: []string{"widget"},
		Gloss: "a different unremarkable thing",
	}
	inv := &fakeInventory{
		senses: map[string][]Synset{
			// Decoy first, so "most frequent sense" would pick the wrong one.
			POSNoun + ":widget": {decoy, target},
		},
		synsets: map[string]Synset{
			"s:parent": {ID: "s:parent", POS: POSNoun, Gloss: "hydraulic mining equipment"},
		},
	}
	terms := []Term{
		{Lemma: "widget", POS: POSNoun},
		{Lemma: "hydraulic", POS: POSNoun},
		{Lemma: "mining", POS: POSNoun},
		{Lemma: "equipment", POS: POSNoun},
	}

	extended, err := Disambiguate(context.Background(), inv, nil, terms, LeskOptions{MaxSenses: 4, MaxRelated: 12})
	if err != nil {
		t.Fatalf("extended: %v", err)
	}
	if got := extended[0].SynsetID; got != "s:target" {
		t.Errorf("extended Lesk chose %q, want %q — the hypernym gloss should have decided it", got, "s:target")
	}

	plain, err := Disambiguate(context.Background(), inv, nil, terms, LeskOptions{MaxSenses: 4, MaxRelated: 0})
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if plain[0].SynsetID == "s:target" {
		t.Errorf("plain Lesk also chose the target; the case does not isolate the extension")
	}
}

func TestDisambiguateFallsBackToMostFrequentSense(t *testing.T) {
	inv := bankInventory()
	// Context shares nothing with either gloss.
	terms := []Term{{Lemma: "bank", POS: POSNoun}}
	senses, err := Disambiguate(context.Background(), inv, nil, terms, DefaultLeskOptions)
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if senses[0].SynsetID != "s:bank-money" {
		t.Errorf("no-overlap fallback chose %q, want the first (most frequent) sense", senses[0].SynsetID)
	}
	if senses[0].Score != 0 {
		t.Errorf("fallback reported score %.1f, want 0 so the caller can tell it was a guess", senses[0].Score)
	}
}

func TestDisambiguateResolvesEachLemmaOnce(t *testing.T) {
	inv := bankInventory()
	terms := []Term{
		{Lemma: "bank", POS: POSNoun}, {Lemma: "bank", POS: POSNoun},
		{Lemma: "bank", POS: POSNoun}, {Lemma: "money", POS: POSNoun},
	}
	senses, err := Disambiguate(context.Background(), inv, nil, terms, DefaultLeskOptions)
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(senses) != 4 {
		t.Fatalf("got %d senses, want one per term", len(senses))
	}
	for i := 0; i < 3; i++ {
		if senses[i].SynsetID != senses[0].SynsetID {
			t.Errorf("repeat %d of the same lemma resolved differently", i)
		}
	}
	// 2 lemmas -> 2 Senses calls; the 3 repeats of "bank" must add none.
	// Anything more means the cache is not working and a metered inventory
	// would be charged for every repetition on a page.
	if inv.calls > 8 {
		t.Errorf("made %d inventory calls for 2 distinct lemmas; repeats are not being reused", inv.calls)
	}
}

func TestUnknownLemmaYieldsNoSense(t *testing.T) {
	inv := bankInventory()
	senses, err := Disambiguate(context.Background(), inv, nil,
		[]Term{{Lemma: "zzzznotaword", POS: POSNoun}}, DefaultLeskOptions)
	if err != nil {
		t.Fatalf("Disambiguate: %v", err)
	}
	if len(senses) != 1 {
		t.Fatalf("got %d senses, want the term kept", len(senses))
	}
	if senses[0].SynsetID != "" {
		t.Errorf("unknown lemma resolved to %q, want empty", senses[0].SynsetID)
	}
}

func TestOverlapSquaresContiguousRuns(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want float64
	}{
		{"no overlap", []string{"x", "y"}, []string{"p", "q"}, 0},
		{"one word", []string{"water"}, []string{"water"}, 1},
		// A three-word phrase scores 9, not 3 — the whole point of the
		// Banerjee & Pedersen weighting.
		{"three-word phrase", []string{"body", "of", "water"}, []string{"a", "body", "of", "water"}, 9},
		// The same three words scattered score 1+1+1.
		{"same words scattered", []string{"body", "x", "of", "y", "water"},
			[]string{"water", "p", "of", "q", "body"}, 3},
		{"two separate runs", []string{"a", "b", "z", "c", "d"}, []string{"a", "b", "q", "c", "d"}, 8},
		{"empty", nil, []string{"a"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlap(tc.a, tc.b); got != tc.want {
				t.Errorf("overlap(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestOverlapIsSymmetric(t *testing.T) {
	a := []string{"sloping", "land", "beside", "a", "body", "of", "water"}
	b := []string{"a", "body", "of", "water", "covering", "sloping", "land"}
	if forward, backward := overlap(a, b), overlap(b, a); forward != backward {
		t.Errorf("overlap is not symmetric: %v vs %v", forward, backward)
	}
}
