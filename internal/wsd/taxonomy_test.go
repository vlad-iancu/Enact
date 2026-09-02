package wsd

import (
	"os"
	"testing"
)

// testTaxonomy loads the real WordNet database, or skips.
//
// These are the only tests here that need the 36 MB download, so they are
// opt-in via WORDNET_DIR rather than a hard requirement — `go test ./...`
// must pass on a clean checkout. Everything algorithmic is covered by the
// fake inventory instead.
func testTaxonomy(t *testing.T) *Taxonomy {
	t.Helper()
	dir := os.Getenv("WORDNET_DIR")
	if dir == "" {
		t.Skip("WORDNET_DIR not set; run `make wordnet` and export it to run this test")
	}
	tax, err := NewTaxonomy(Config{WordNetDir: dir})
	if err != nil {
		t.Fatalf("NewTaxonomy: %v", err)
	}
	return tax
}

func TestNormalizeWordNetKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"wn:02084071n", "n02084071"},
		{"02084071n", "n02084071"},
		{"n02084071", "n02084071"},
		// Adjective satellites live in the adjective files.
		{"wn:01234567s", "a01234567"},
		{"bn:00008364n", ""}, // a BabelNet id is not a WordNet reference
		{"", ""},
		{"wn:", ""},
		{"nonsense", ""},
	}
	for _, tc := range cases {
		if got := NormalizeWordNetKey(tc.in); got != tc.want {
			t.Errorf("NormalizeWordNetKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSimilarityOrdersConcepts(t *testing.T) {
	tax := testTaxonomy(t)
	// Sense keys taken from WordNet 3.0: the animal senses, not the
	// "contemptible person" or "CAT scan" ones. Picking the right sense is
	// the entire reason this package disambiguates before it measures.
	const (
		dog       = "n02084071" // domestic dog
		cat       = "n02121620" // true cat
		bankSlope = "n09213565" // sloping land beside water
	)
	same := tax.Similarity(dog, dog)
	related := tax.Similarity(dog, cat)
	unrelated := tax.Similarity(dog, bankSlope)

	if same != 1 {
		t.Errorf("similarity of a synset with itself = %.3f, want 1", same)
	}
	if !(related > unrelated) {
		t.Errorf("wup(dog,cat)=%.3f must exceed wup(dog,bank)=%.3f", related, unrelated)
	}
	if related < 0.5 {
		t.Errorf("wup(dog,cat)=%.3f is implausibly low; they share the carnivore ancestry", related)
	}
	t.Logf("wup: same=%.3f dog/cat=%.3f dog/bank=%.3f", same, related, unrelated)
}

func TestSimilarityHandlesMissingKeys(t *testing.T) {
	tax := testTaxonomy(t)
	// A BabelNet-only sense (a named entity, say) has no WordNet key. It must
	// score 0 rather than panic or error — the BM25 half of the score is what
	// covers such pages.
	if got := tax.Similarity("", "n02084071"); got != 0 {
		t.Errorf("similarity with an empty key = %.3f, want 0", got)
	}
	if got := tax.Similarity("n99999999", "n02084071"); got != 0 {
		t.Errorf("similarity with an unknown key = %.3f, want 0", got)
	}
	// Nouns and verbs are separate hierarchies with no shared root.
	if got := tax.Similarity("n02084071", "v02084071"); got != 0 {
		t.Errorf("cross-part-of-speech similarity = %.3f, want 0", got)
	}
}

func TestLemmatize(t *testing.T) {
	tax := testTaxonomy(t)
	cases := []struct {
		word, pos, want string
	}{
		{"dogs", POSNoun, "dog"},
		{"boxes", POSNoun, "box"},
		{"cities", POSNoun, "city"},
		// Irregulars, which only the exception lists know.
		{"geese", POSNoun, "goose"},
		{"mice", POSNoun, "mouse"},
		{"ran", POSVerb, "run"},
		{"went", POSVerb, "go"},
		{"running", POSVerb, "run"},
		{"better", POSAdjective, "good"},
		// Already a lemma: unchanged.
		{"dog", POSNoun, "dog"},
		{"water", POSNoun, "water"},
		// The Morphy trap: detaching "s" from "bus" gives "bu", which is not
		// a word, so the original must survive.
		{"bus", POSNoun, "bus"},
	}
	for _, tc := range cases {
		if got := tax.Lemmatize(tc.word, tc.pos); got != tc.want {
			t.Errorf("Lemmatize(%q, %q) = %q, want %q", tc.word, tc.pos, got, tc.want)
		}
	}
}

func TestAnalyzeKeepsContentWords(t *testing.T) {
	tax := testTaxonomy(t)
	terms, err := tax.Analyze("The banks were quickly raising their interest rates on deposits.")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := map[string]string{}
	for _, term := range terms {
		got[term.Lemma] = term.POS
	}
	// Content words present and lemmatised.
	for lemma, pos := range map[string]string{
		"bank": POSNoun, "rate": POSNoun, "deposit": POSNoun,
	} {
		if got[lemma] != pos {
			t.Errorf("expected %q tagged %q, got %q (terms: %v)", lemma, pos, got[lemma], terms)
		}
	}
	// Function words dropped.
	for _, dropped := range []string{"the", "were", "their", "on", "quickly"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("%q should have been dropped as a non-content word", dropped)
		}
	}
}

func TestSalientRanksByFrequencyAndIDF(t *testing.T) {
	terms := []Term{
		{Lemma: "otter", POS: POSNoun}, {Lemma: "otter", POS: POSNoun},
		{Lemma: "otter", POS: POSNoun},
		{Lemma: "page", POS: POSNoun}, {Lemma: "page", POS: POSNoun},
		{Lemma: "sea", POS: POSNoun},
	}
	// No idf: pure frequency.
	byFreq := Salient(terms, nil, 2)
	if len(byFreq) != 2 || byFreq[0].Lemma != "otter" || byFreq[1].Lemma != "page" {
		t.Errorf("frequency ranking = %v, want otter then page", byFreq)
	}
	// With idf, a common word is demoted even though it is frequent here.
	idf := func(lemma string) float64 {
		if lemma == "page" {
			return 0.01
		}
		return 1
	}
	byIDF := Salient(terms, idf, 2)
	if len(byIDF) != 2 || byIDF[0].Lemma != "otter" || byIDF[1].Lemma != "sea" {
		t.Errorf("tf-idf ranking = %v, want otter then sea (page demoted)", byIDF)
	}
	if got := Salient(terms, nil, 0); got != nil {
		t.Errorf("Salient(n=0) = %v, want nil", got)
	}
	if got := Salient(terms, nil, 100); len(got) != 3 {
		t.Errorf("Salient(n > distinct) returned %d, want the 3 distinct terms", len(got))
	}
}
