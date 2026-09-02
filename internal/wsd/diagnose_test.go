package wsd

import (
	"context"
	"strings"
	"testing"
)

// TestDiagnoseSeparatesSearchFromObjective is the tool's reason to exist: it
// has to be able to tell the two failure modes apart, or it is just another
// way of printing the answer.
func TestDiagnoseSeparatesSearchFromObjective(t *testing.T) {
	ctxBag := []string{"index", "security", "cluster"}

	// A colony with its usual budget solves this three-word case exactly.
	full, err := Diagnose(context.Background(), technicalInventory(), nil,
		technicalTerms(), ctxBag, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}, 0)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if !full.Exhaustive {
		t.Fatalf("the search space was %d; this fixture must be small enough to enumerate", full.SearchSpace)
	}
	optimal, known := full.SearchIsOptimal()
	if !known || !optimal {
		t.Errorf("SearchIsOptimal = (%v, %v), want the well-fed colony to reach the optimum "+
			"(colony %.4f, optimum %.4f)", optimal, known, full.ColonyScore, full.OptimumScore)
	}

	// One ant, one cycle, and a prior that pulls hard towards the dictionary's
	// first sense: a colony that cannot search. The diagnosis has to notice.
	starved, err := Diagnose(context.Background(), technicalInventory(), nil,
		technicalTerms(), ctxBag, ACOOptions{
			Lesk: LeskOptions{MaxSenses: 4}, Ants: 1, Cycles: 1, PriorWeight: 50, Beta: 8,
		}, 0)
	if err != nil {
		t.Fatalf("Diagnose starved: %v", err)
	}
	if starved.ColonyScore > starved.OptimumScore+1e-9 {
		t.Fatalf("colony %.4f beat the exhaustive optimum %.4f, which is impossible; "+
			"the two are not scoring the same objective",
			starved.ColonyScore, starved.OptimumScore)
	}
	if optimal, known := starved.SearchIsOptimal(); known && optimal {
		t.Log("the starved colony still found the optimum; the fixture is too easy to fail on")
	}
}

// TestDiagnoseExplainsWhatTheScoreIsMadeOf covers the other half. A number
// says a sense won; the shared words say why, and that is what turned three
// vague suspicions into three fixed defects.
func TestDiagnoseExplainsWhatTheScoreIsMadeOf(t *testing.T) {
	d, err := Diagnose(context.Background(), technicalInventory(), nil, technicalTerms(),
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}, 0)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(d.Pairs) == 0 {
		t.Fatal("no pair contributions; the decomposition is empty")
	}
	// Sorted strongest first, so the thing to look at is the thing at the top.
	for i := 1; i < len(d.Pairs); i++ {
		if d.Pairs[i-1].Relatedness < d.Pairs[i].Relatedness {
			t.Errorf("pair %d scored below pair %d; the decomposition is not ordered", i-1, i)
		}
	}
	var shared []string
	for _, pair := range d.Pairs {
		shared = append(shared, pair.Shared...)
	}
	if len(shared) == 0 {
		t.Error("no shared words recorded; the decomposition cannot explain any score")
	}
	// The fixture's technical senses agree on "database system"; if the
	// evidence trail does not mention it, it is not tracing the real overlap.
	if !strings.Contains(strings.Join(shared, " "), "database") {
		t.Errorf("shared words %v never mention the phrase the senses actually agree on", shared)
	}

	// Every term must be accounted for, including a word with no senses —
	// which is a common and easily missed cause of a bad crawl.
	if len(d.Terms) != 3 {
		t.Errorf("got %d terms, want one per distinct word", len(d.Terms))
	}
	for _, term := range d.Terms {
		if len(term.Candidates) == 0 {
			continue
		}
		if term.ColonyPick < 0 || term.ColonyPick >= len(term.Candidates) {
			t.Errorf("%s: ColonyPick %d is not a candidate index", term.Term.Lemma, term.ColonyPick)
		}
		for _, candidate := range term.Candidates {
			if candidate.GlossSize == 0 {
				t.Errorf("%s/%s: gloss size 0; the size a sense won on must be visible",
					term.Term.Lemma, candidate.SynsetID)
			}
		}
	}
}

// TestDiagnoseScoresAnExpectedReading turns "it picked the wrong sense" into a
// number, which is the difference between an opinion and a bug report.
func TestDiagnoseScoresAnExpectedReading(t *testing.T) {
	d, err := Diagnose(context.Background(), technicalInventory(), nil, technicalTerms(),
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}, 0)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	// The everyday reading, which this fixture is built so the colony rejects.
	score, matched := d.ScoreOf([]string{"s:index-sign", "s:security-collateral", "s:cluster-bunch"})
	if matched != 3 {
		t.Fatalf("matched %d of 3 named senses", matched)
	}
	if score >= d.OptimumScore {
		t.Errorf("the rejected reading scores %.4f against the optimum's %.4f; "+
			"it must score lower, or the objective did not actually prefer the other one",
			score, d.OptimumScore)
	}
}

// TestDiagnoseDeclinesToGuessWhenTheSpaceIsTooLarge keeps the tool honest: an
// unknown optimum must be reported as unknown, never as agreement.
func TestDiagnoseDeclinesToGuessWhenTheSpaceIsTooLarge(t *testing.T) {
	d, err := Diagnose(context.Background(), technicalInventory(), nil, technicalTerms(),
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}, 4)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Exhaustive {
		t.Fatal("Exhaustive = true with a cap of 4 assignments")
	}
	if _, known := d.SearchIsOptimal(); known {
		t.Error("SearchIsOptimal claims to know the answer without having enumerated it")
	}
}

// TestDiagnoseRefusesAVerdictOnAnEmptyInventory guards the tool against its
// own worst failure. When the dictionary has refused the request there is
// nothing to judge, and saying "the search is fine" sends the reader after the
// scoring function for a problem that is an expired API key. Found by pointing
// the tool at BabelNet with the daily allowance spent.
func TestDiagnoseRefusesAVerdictOnAnEmptyInventory(t *testing.T) {
	exhausted := &budgetedInventory{
		fakeInventory: technicalInventory(), limit: 0,
		err: ErrInventoryExhausted,
	}
	d, _ := Diagnose(context.Background(), exhausted, nil, technicalTerms(),
		[]string{"index", "security", "cluster"}, ACOOptions{Lesk: LeskOptions{MaxSenses: 4}}, 0)
	if d == nil {
		t.Fatal("Diagnose returned nothing; a partial diagnosis is still worth reading")
	}
	if got := d.Resolved(); got != 0 {
		t.Errorf("Resolved = %d, want 0", got)
	}
	// Every word of the query must still be listed, or a truncated analysis
	// reads as a complete one over a shorter query.
	if len(d.Terms) != 3 {
		t.Errorf("got %d terms, want all 3 including the ones never reached", len(d.Terms))
	}
	if _, known := d.SearchIsOptimal(); known {
		t.Error("SearchIsOptimal delivered a verdict over an empty search")
	}
	var report strings.Builder
	d.Report(&report, nil)
	if strings.Contains(report.String(), "THE SEARCH IS FINE") {
		t.Errorf("the report exonerates the search over an empty inventory:\n%s", report.String())
	}
	if !strings.Contains(report.String(), "RESOLVED 0 OF 3") {
		t.Errorf("the report does not say the inventory resolved nothing:\n%s", report.String())
	}
}
