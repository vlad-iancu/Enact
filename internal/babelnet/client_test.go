package babelnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"enact/internal/wsd"
)

// fakeBabelNet serves the three endpoints with the shapes babelnet.io
// documents, and counts requests so the budget and cache can be asserted on.
type fakeBabelNet struct {
	*httptest.Server
	calls map[string]int
	// unauthorized makes every call answer 401, as BabelNet does when the
	// day's coins are gone.
	unauthorized bool
}

func newFakeBabelNet(t *testing.T) *fakeBabelNet {
	t.Helper()
	f := &fakeBabelNet{calls: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/getSynsetIds", func(w http.ResponseWriter, r *http.Request) {
		f.calls["getSynsetIds"]++
		if f.unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("key") == "" {
			t.Errorf("getSynsetIds called without an API key")
		}
		switch r.URL.Query().Get("lemma") {
		case "otter":
			writeJSON(w, []map[string]string{
				{"id": "bn:00062164n", "pos": "NOUN", "source": "BABELNET"},
			})
		default:
			writeJSON(w, []map[string]string{})
		}
	})

	mux.HandleFunc("/getSynset", func(w http.ResponseWriter, r *http.Request) {
		f.calls["getSynset"]++
		if f.unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The shapes below are copied from a real babelnet.io v9 response for
		// bn:00059723n. Two of them were originally guessed wrong here and
		// only surfaced against the live API:
		//
		//   - wnOffsets is an array of OBJECTS, not strings, and carries one
		//     entry per WordNet version. The OEWN entry is listed first and is
		//     useless to us — the local taxonomy is WordNet 3.0 — so taking
		//     [0] silently zeroed every similarity.
		//   - a Wikipedia-derived lemma keeps its article disambiguator,
		//     "otter_(fishing_device)", which is not a word any page contains.
		writeJSON(w, map[string]any{
			"senses": []map[string]any{
				{"properties": map[string]any{"fullLemma": "otter", "language": "EN", "pos": "NOUN"}},
				{"properties": map[string]any{"fullLemma": "otter_(fishing_device)", "language": "EN", "pos": "NOUN"}},
				{"properties": map[string]any{"fullLemma": "Fischotter", "language": "DE", "pos": "NOUN"}},
			},
			"glosses": []map[string]any{
				{"gloss": "freshwater carnivorous mammal", "language": "EN"},
				{"gloss": "ein Marder", "language": "DE"},
			},
			"wnOffsets": []map[string]any{
				{"versionMapping": map[string]any{}, "version": "OEWN", "id": "oewn:02483504n", "pos": "NOUN", "source": "OEWN"},
				{"versionMapping": map[string]any{}, "version": "WN_30", "id": "wn:02444819n", "pos": "NOUN", "source": "WN"},
			},
		})
	})

	mux.HandleFunc("/getOutgoingEdges", func(w http.ResponseWriter, r *http.Request) {
		f.calls["getOutgoingEdges"]++
		if f.unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, []map[string]any{
			{"target": "bn:00046516n", "pointer": map[string]any{"relationGroup": "HYPERNYM", "name": "Hypernym"}},
			{"target": "bn:00007309n", "pointer": map[string]any{"relationGroup": "HYPONYM", "name": "Hyponym"}},
			{"target": "bn:00001111n", "pointer": map[string]any{"relationGroup": "", "name": "Derivationally related form"}},
			{"target": "bn:00002222n", "pointer": map[string]any{"relationGroup": "SOMETHING_ELSE", "name": "Whatever"}},
		})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testInventory wires the real client to the fake server, with a cache and
// budget that never touch OpenSearch: both degrade to their in-process layer
// when the store is absent, which is what makes them testable here and what
// keeps a transient OpenSearch outage from failing a crawl in production.
func testInventory(t *testing.T, f *fakeBabelNet, dailyBudget int) *Inventory {
	t.Helper()
	cfg := Config{
		APIKey: "test-key", BaseURL: f.URL, SearchLang: "EN",
		DailyBudget: dailyBudget, CacheIndex: "test-cache", MaxSenses: 4,
	}
	inv := New(cfg, nil, nil)
	// Neuter persistence: these tests are about the algorithms around the
	// cache, not about OpenSearch.
	inv.cache.os = nil
	inv.budget.os = nil
	return inv
}

func TestSynsetDecodesTheDocumentedShape(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 100)

	got, err := inv.Synset(context.Background(), "bn:00062164n")
	if err != nil {
		t.Fatalf("Synset: %v", err)
	}
	if got.ID != "bn:00062164n" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.POS != wsd.POSNoun {
		t.Errorf("POS = %q, want %q (read off the id suffix)", got.POS, wsd.POSNoun)
	}
	// Only the search language survives, and the Wikipedia disambiguator is
	// stripped so both English senses reduce to the bare word.
	if len(got.Lemmas) != 2 || got.Lemmas[0] != "otter" || got.Lemmas[1] != "otter" {
		t.Errorf("Lemmas = %v, want the two English ones with decorations stripped", got.Lemmas)
	}
	if got.Gloss != "freshwater carnivorous mammal" {
		t.Errorf("Gloss = %q, want the English gloss", got.Gloss)
	}
	// The WordNet offset is what makes this sense measurable on the taxonomy.
	if got.WordNetKey != "n02444819" {
		t.Errorf("WordNetKey = %q, want %q", got.WordNetKey, "n02444819")
	}
	wantRelations := map[string]string{
		"bn:00046516n": wsd.RelationHypernym,
		"bn:00007309n": wsd.RelationHyponym,
		"bn:00001111n": wsd.RelationDerivation,
		"bn:00002222n": wsd.RelationOther,
	}
	if len(got.Relations) != len(wantRelations) {
		t.Fatalf("got %d relations, want %d", len(got.Relations), len(wantRelations))
	}
	for _, rel := range got.Relations {
		if want := wantRelations[rel.Target]; rel.Type != want {
			t.Errorf("relation to %s typed %q, want %q", rel.Target, rel.Type, want)
		}
	}
}

func TestCacheAvoidsRepeatRequests(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 100)
	ctx := context.Background()

	if _, err := inv.Synset(ctx, "bn:00062164n"); err != nil {
		t.Fatalf("first Synset: %v", err)
	}
	firstSynset, firstEdges := f.calls["getSynset"], f.calls["getOutgoingEdges"]
	if firstSynset != 1 || firstEdges != 1 {
		t.Fatalf("a cold synset cost getSynset=%d getOutgoingEdges=%d, want 1 each", firstSynset, firstEdges)
	}
	for i := 0; i < 5; i++ {
		if _, err := inv.Synset(ctx, "bn:00062164n"); err != nil {
			t.Fatalf("repeat Synset: %v", err)
		}
	}
	if f.calls["getSynset"] != firstSynset || f.calls["getOutgoingEdges"] != firstEdges {
		t.Errorf("five repeats cost more requests: getSynset=%d getOutgoingEdges=%d",
			f.calls["getSynset"], f.calls["getOutgoingEdges"])
	}
	if spent := inv.Spent(); spent != 2 {
		t.Errorf("budget spent = %d, want 2 — repeats must not be charged", spent)
	}
}

// TestSynsetGlossSkipsEdges pins the optimisation that halves extended Lesk's
// neighbour cost.
func TestSynsetGlossSkipsEdges(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 100)

	got, err := inv.SynsetGloss(context.Background(), "bn:00062164n")
	if err != nil {
		t.Fatalf("SynsetGloss: %v", err)
	}
	if got.Gloss == "" {
		t.Error("SynsetGloss returned no gloss, which is the only thing it is for")
	}
	if len(got.Relations) != 0 {
		t.Errorf("SynsetGloss returned %d relations, want none", len(got.Relations))
	}
	if f.calls["getOutgoingEdges"] != 0 {
		t.Errorf("SynsetGloss made %d edge requests, want 0", f.calls["getOutgoingEdges"])
	}
	if spent := inv.Spent(); spent != 1 {
		t.Errorf("budget spent = %d, want 1", spent)
	}
}

func TestBudgetStopsSpendingAndIsASentinel(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 2) // enough for exactly one cold synset
	ctx := context.Background()

	if _, err := inv.Synset(ctx, "bn:00062164n"); err != nil {
		t.Fatalf("first synset should fit the budget: %v", err)
	}
	_, err := inv.Synset(ctx, "bn:00046516n")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second synset error = %v, want ErrBudgetExhausted", err)
	}
	// The point of the sentinel: the caller can tell "out of coins" from "the
	// network broke", and only the former means "stop and resume tomorrow".
	before := f.calls["getSynset"]
	if _, err := inv.Synset(ctx, "bn:00099999n"); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("further calls should keep refusing, got %v", err)
	}
	if f.calls["getSynset"] != before {
		t.Errorf("a refused call still reached the network (%d -> %d)", before, f.calls["getSynset"])
	}
	if inv.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0", inv.Remaining())
	}
}

// TestRefusalIsTreatedAsExhaustion: BabelNet answers one 403 for both a bad
// key and a spent allowance — its own message is "Your key is not valid or
// the daily requests limit has been reached." Either way the crawl stops.
func TestRefusalIsTreatedAsExhaustion(t *testing.T) {
	f := newFakeBabelNet(t)
	f.unauthorized = true
	inv := testInventory(t, f, 100)

	_, err := inv.Synset(context.Background(), "bn:00062164n")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("a refusal produced %v, want it to satisfy ErrBudgetExhausted", err)
	}
}

// TestRefusalOnAFreshDayBlamesTheKey covers the diagnosis the operator
// actually needs. An allowance cannot have been spent by requests this
// deployment never made, so a refusal on the day's first request points at
// the key — otherwise a typo in BABELNET_API_KEY reports "budget exhausted"
// every day forever and nobody can tell why.
func TestRefusalOnAFreshDayBlamesTheKey(t *testing.T) {
	f := newFakeBabelNet(t)
	f.unauthorized = true
	inv := testInventory(t, f, 100)

	_, err := inv.Synset(context.Background(), "bn:00062164n")
	if !errors.Is(err, ErrKeyRejected) {
		t.Errorf("first-request refusal = %v, want ErrKeyRejected", err)
	}
	// Still stops the crawl, so callers need not distinguish the two.
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("ErrKeyRejected must wrap ErrBudgetExhausted, got %v", err)
	}
}

// TestRefusalLaterInTheDayBlamesTheBudget is the other side: once the
// counter shows real spending, exhaustion is the better explanation.
func TestRefusalLaterInTheDayBlamesTheBudget(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 100)
	ctx := context.Background()

	// Spend a few requests successfully first.
	if _, err := inv.Synset(ctx, "bn:00062164n"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if inv.Spent() < 2 {
		t.Fatalf("warm-up spent %d requests, want at least 2", inv.Spent())
	}
	f.unauthorized = true
	_, err := inv.Synset(ctx, "bn:00046516n")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("later refusal = %v, want ErrBudgetExhausted", err)
	}
	if errors.Is(err, ErrKeyRejected) {
		t.Errorf("a refusal after real spending should not blame the key: %v", err)
	}
}

func TestSensesResolvesEachCandidate(t *testing.T) {
	f := newFakeBabelNet(t)
	inv := testInventory(t, f, 100)

	senses, err := inv.Senses(context.Background(), "otter", wsd.POSNoun)
	if err != nil {
		t.Fatalf("Senses: %v", err)
	}
	if len(senses) != 1 {
		t.Fatalf("got %d senses, want 1", len(senses))
	}
	if senses[0].Gloss == "" || len(senses[0].Relations) == 0 {
		t.Error("a candidate sense must arrive with its gloss and relations, which is what Lesk needs")
	}
	// An unknown lemma is not an error.
	none, err := inv.Senses(context.Background(), "zzzznotaword", wsd.POSNoun)
	if err != nil {
		t.Fatalf("unknown lemma: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown lemma returned %d senses, want 0", len(none))
	}
}

func TestMissingAPIKeyIsReported(t *testing.T) {
	inv := New(Config{BaseURL: "http://127.0.0.1:1", SearchLang: "EN", DailyBudget: 10}, nil, nil)
	inv.cache.os = nil
	inv.budget.os = nil
	_, err := inv.Synset(context.Background(), "bn:00062164n")
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("error = %v, want ErrNoAPIKey", err)
	}
}

// TestWordNet30KeyIsPickedByVersion guards the bug that would have been
// invisible: BabelNet lists an Open English WordNet id BEFORE the WordNet 3.0
// one, and the local taxonomy only has 3.0. Taking the first entry produces a
// well-formed key that resolves to nothing, so every semantic similarity
// silently becomes 0 and the crawl degrades to BM25 with no error anywhere.
func TestWordNet30KeyIsPickedByVersion(t *testing.T) {
	cases := []struct {
		name    string
		offsets []wnOffset
		want    string
	}{
		{
			name: "prefers WN_30 over the OEWN entry listed first",
			offsets: []wnOffset{
				{ID: "oewn:02483504n", Source: "OEWN", Version: "OEWN"},
				{ID: "wn:02444819n", Source: "WN", Version: "WN_30"},
			},
			want: "n02444819",
		},
		{
			name:    "no 3.0 mapping yields nothing rather than a dead id",
			offsets: []wnOffset{{ID: "oewn:02483504n", Source: "OEWN", Version: "OEWN"}},
			want:    "",
		},
		{name: "no offsets at all", offsets: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wordNet30Key(tc.offsets); got != tc.want {
				t.Errorf("wordNet30Key = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCleanLemma(t *testing.T) {
	cases := map[string]string{
		"otter":                  "otter",
		"otter_(fishing_device)": "otter",
		"sea_otter":              "sea_otter", // underscores between words stay; only the disambiguator goes
		"  otter  ":              "otter",
		"":                       "",
	}
	for in, want := range cases {
		if got := cleanLemma(in); got != want {
			t.Errorf("cleanLemma(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOrderSensesPutsDictionarySensesFirst covers the other live-API finding:
// BabelNet does not order senses by frequency. For "otter" the animal is the
// fifth of twenty-five, behind a fishing device, a heraldic charge, a
// steamship and a town — so a naive cap would keep four senses and discard
// the only one anybody meant.
func TestOrderSensesPutsDictionarySensesFirst(t *testing.T) {
	senses := []wsd.Synset{
		{ID: "bn:16283359n"},                          // fishing device, Wikipedia only
		{ID: "bn:05302479n"},                          // heraldic animal
		{ID: "bn:00059723n", WordNetKey: "n02444819"}, // the animal
		{ID: "bn:00950202n"},                          // steamship
		{ID: "bn:00059722n", WordNetKey: "n14765785"}, // otter fur
	}
	got := orderSenses(senses)
	if got[0].ID != "bn:00059723n" || got[1].ID != "bn:00059722n" {
		t.Errorf("dictionary senses did not come first: %v", ids(got))
	}
	if len(got) != len(senses) {
		t.Errorf("ordering dropped senses: %d -> %d", len(senses), len(got))
	}
	// Relative order within each group is preserved, so BabelNet's own
	// ranking still decides ties.
	if got[2].ID != "bn:16283359n" || got[3].ID != "bn:05302479n" {
		t.Errorf("relative order of the Wikipedia senses changed: %v", ids(got))
	}
	// A term with only entity senses keeps them all — there is nothing better.
	onlyEntities := []wsd.Synset{{ID: "bn:03083790n"}, {ID: "bn:01234567n"}}
	if got := orderSenses(onlyEntities); len(got) != 2 || got[0].ID != "bn:03083790n" {
		t.Errorf("entity-only senses were reordered or dropped: %v", ids(got))
	}
}

func ids(senses []wsd.Synset) []string {
	out := make([]string, len(senses))
	for i, s := range senses {
		out[i] = s.ID
	}
	return out
}

// TestRelationsAreOrderedMostInformativeFirst covers the third live-API
// finding. The real synset for the animal "otter" has 1739 relations: 8
// hypernyms, 13 hyponyms, 2 holonyms — and 1716 ungrouped relatedness edges,
// which arrive first. Every consumer here takes a bounded prefix (extended
// Lesk's MaxRelated, expansion's MaxFanout), so arrival order would spend the
// entire budget on the 1716 and never see the taxonomy.
func TestRelationsAreOrderedMostInformativeFirst(t *testing.T) {
	// Mirrors the real distribution: noise first, signal buried.
	edges := make([]edgeResponse, 0, 30)
	add := func(group, name, target string) {
		e := edgeResponse{Target: target}
		e.Pointer.RelationGroup = group
		e.Pointer.Name = name
		edges = append(edges, e)
	}
	for i := 0; i < 25; i++ {
		add("SOMETHING_ELSE", "Related", "bn:9000000"+string(rune('a'+i))+"n")
	}
	add("HYPERNYM", "", "bn:hyper1n")
	add("HYPONYM", "", "bn:hypo1n")
	add("MERONYM", "", "bn:mero1n")
	add("", "Derivationally related form", "bn:deriv1n")
	add("HYPERNYM", "", "bn:hyper2n")

	got := toSynset("bn:00059723n", synsetResponse{}, edges, "EN")
	if len(got.Relations) != len(edges) {
		t.Fatalf("got %d relations, want all %d kept", len(got.Relations), len(edges))
	}
	// The taxonomic edges must all be inside a MaxFanout-sized window.
	window := got.Relations[:wsd.DefaultMaxFanout]
	counts := map[string]int{}
	for _, rel := range window {
		counts[rel.Type]++
	}
	if counts[wsd.RelationHypernym] != 2 {
		t.Errorf("only %d of 2 hypernyms are inside the fan-out window: %v", counts[wsd.RelationHypernym], counts)
	}
	if counts[wsd.RelationHyponym] != 1 || counts[wsd.RelationMeronym] != 1 || counts[wsd.RelationDerivation] != 1 {
		t.Errorf("taxonomic edges missing from the fan-out window: %v", counts)
	}
	// Hypernyms lead, because they place a concept most cheaply.
	if got.Relations[0].Type != wsd.RelationHypernym {
		t.Errorf("first relation is %q, want a hypernym", got.Relations[0].Type)
	}
}

func TestRelationTypeMapping(t *testing.T) {
	cases := []struct{ group, name, want string }{
		{"HYPERNYM", "", wsd.RelationHypernym},
		{"hypernym", "", wsd.RelationHypernym},
		{"HYPONYM", "", wsd.RelationHyponym},
		{"MERONYM", "", wsd.RelationMeronym},
		{"HOLONYM", "", wsd.RelationHolonym},
		{"", "Derivationally related form", wsd.RelationDerivation},
		{"ANTONYM", "Antonym", wsd.RelationOther},
		{"", "", wsd.RelationOther},
	}
	for _, tc := range cases {
		if got := relationType(tc.group, tc.name); got != tc.want {
			t.Errorf("relationType(%q, %q) = %q, want %q", tc.group, tc.name, got, tc.want)
		}
	}
}

func TestBabelPOSMapping(t *testing.T) {
	cases := map[string]string{
		wsd.POSNoun: "NOUN", wsd.POSVerb: "VERB",
		wsd.POSAdjective: "ADJ", wsd.POSAdverb: "ADV", "zz": "",
	}
	for in, want := range cases {
		if got := babelPOS(in); got != want {
			t.Errorf("babelPOS(%q) = %q, want %q", in, got, want)
		}
	}
}
