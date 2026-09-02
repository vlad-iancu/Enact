package wsd

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// DefaultMaxAssignments bounds the exhaustive search.
//
// Two million assignments of a dozen words is a second or so, and the point of
// diminishing returns: past it, the question the brute force answers — is the
// colony finding the optimum — is better answered on a smaller query.
const DefaultMaxAssignments = 2_000_000

// Diagnosis explains one disambiguation completely enough to debug it.
//
// It exists because a wrong sense has two utterly different causes that look
// identical from outside: the search failed to find the best assignment, or it
// found it and the best assignment is wrong. Chasing the first when it is
// really the second means tuning ant counts and evaporation rates for days
// against a scoring function that was never going to give the right answer.
//
// So the diagnosis reports both. Exhaustive enumeration — when the space is
// small enough, which for a real query it usually is — says definitively
// whether the colony is optimising well, and the decomposition says what the
// objective is actually rewarding. Both were needed to find the three defects
// recorded in aco_test.go: every one of them was in the objective, and the
// colony had been hitting the exact optimum the whole time.
type Diagnosis struct {
	// Terms is every word, its candidates, and what each of them scored.
	Terms []TermDiagnosis
	// ContextSize is how many context words the affinities were measured
	// against.
	ContextSize int

	// ColonyScore is what DisambiguateACO's answer scores under the objective.
	ColonyScore float64
	// OptimumScore is the best any assignment scores. Meaningful only when
	// Exhaustive; otherwise it equals ColonyScore.
	OptimumScore float64
	// Exhaustive says whether every assignment was enumerated. When false the
	// search space exceeded the cap and the optimum is unknown.
	Exhaustive bool
	// SearchSpace is how many complete assignments exist.
	SearchSpace int

	// Pairs decomposes the winning assignment into the sense-to-sense
	// agreements that produced its score, with the words they agreed on. This
	// is where a score built out of coincidences becomes visible.
	Pairs []PairContribution
	// ContextTotal and PairTotal are the two halves of the objective. A
	// context total near zero means the surrounding text — the seed pages, for
	// a crawl — decided nothing, whatever weight it was nominally given.
	ContextTotal float64
	PairTotal    float64

	// colony is kept so ScoreOf can evaluate an arbitrary assignment against
	// the real objective rather than a reconstruction of it.
	colony *colony
}

// TermDiagnosis is one word and the choice made for it.
type TermDiagnosis struct {
	Term Term
	// Candidates in the order the inventory offered them, which for a
	// dictionary that orders by corpus frequency is most-frequent first.
	Candidates []CandidateDiagnosis
	// ColonyPick and OptimumPick index Candidates, or -1 when the inventory
	// knows no sense for the word at all — which is itself a common and easily
	// missed explanation for a bad crawl.
	ColonyPick  int
	OptimumPick int
}

// CandidateDiagnosis is one possible sense and the evidence for it.
type CandidateDiagnosis struct {
	SynsetID string
	Gloss    string
	// GlossSize is how many tokens the extended gloss came to. Worth showing:
	// a sense with far more text than its rivals used to win on bulk alone,
	// which is what relatedness() now corrects for.
	GlossSize int
	// Affinity is agreement with the surrounding text, and ContextMatches the
	// words it agreed on. When the matches are all furniture, the context is
	// contributing noise rather than evidence.
	Affinity       float64
	ContextMatches []string
}

// PairContribution is how much two chosen senses agreed, and on what.
type PairContribution struct {
	A, B        Term
	Relatedness float64
	Shared      []string
}

// SearchIsOptimal reports whether the colony found the best assignment.
//
// False is a search problem — more ants, more cycles, slower evaporation. True
// with a wrong answer is a modelling problem, and no amount of tuning will
// help. Unknown when the space was too large to enumerate.
func (d *Diagnosis) SearchIsOptimal() (optimal, known bool) {
	// Nothing to search is not a search that succeeded. Without this the tool
	// answers "the search is fine" over an inventory that resolved not one
	// word — which is the single most misleading thing it could say, because
	// it points at the objective when the real problem is that the dictionary
	// refused the request.
	// Fewer than two resolved words is not a search: there are no pairs to
	// trade off, so "the colony found the optimum" is true and says nothing.
	// Reporting it as a verdict sends the reader after the objective when the
	// dictionary is what failed.
	if !d.Exhaustive || d.Resolved() < 2 {
		return false, false
	}
	// Floating point: the colony's assignment is compared by score, not by
	// identity, because two different assignments can tie.
	return d.ColonyScore >= d.OptimumScore-1e-9, true
}

func seenTerm(list []TermDiagnosis, term Term) bool {
	for _, existing := range list {
		if existing.Term.Lemma == term.Lemma && existing.Term.POS == term.POS {
			return true
		}
	}
	return false
}

// Resolved is how many words the inventory offered any sense at all for.
//
// Reported separately because zero is a complete explanation on its own, and
// one that looks nothing like a scoring problem: a metered dictionary that has
// refused the request resolves nothing, and every downstream number is then
// vacuously zero rather than wrong.
func (d *Diagnosis) Resolved() int {
	n := 0
	for _, term := range d.Terms {
		if len(term.Candidates) > 0 {
			n++
		}
	}
	return n
}

// Diagnose runs the disambiguation and explains it.
//
// It uses the production colony rather than a copy of it. That is the whole
// discipline of the thing: a harness that reimplements the objective drifts
// from the code it is supposed to be measuring, and then confidently explains
// a system that no longer exists.
func Diagnose(ctx context.Context, inv Inventory, tax *Taxonomy,
	terms []Term, contextBag []string, opts ACOOptions, maxAssignments int) (*Diagnosis, error) {

	opts = opts.withDefaults()
	if maxAssignments <= 0 {
		maxAssignments = DefaultMaxAssignments
	}
	c, err := newColony(ctx, inv, tax, terms, contextBag, opts)
	if c == nil {
		return nil, err
	}
	c.seal()

	d := &Diagnosis{ContextSize: len(contextBag), colony: c}
	colony := c.run()
	d.ColonyScore = c.objective(colony)

	optimum, optimumScore, space, exhaustive := c.enumerate(maxAssignments)
	d.SearchSpace, d.Exhaustive = space, exhaustive
	if !exhaustive {
		optimum, optimumScore = colony, d.ColonyScore
	}
	d.OptimumScore = optimumScore

	for i, w := range c.words {
		td := TermDiagnosis{Term: w.term, ColonyPick: -1, OptimumPick: -1}
		if i < len(colony) {
			td.ColonyPick = colony[i]
		}
		if i < len(optimum) {
			td.OptimumPick = optimum[i]
		}
		own := withoutLemma(contextBag, w.term.Lemma)
		for j, candidate := range w.candidates {
			td.Candidates = append(td.Candidates, CandidateDiagnosis{
				SynsetID: candidate.ID, Gloss: candidate.Gloss,
				GlossSize: len(w.glosses[j]), Affinity: w.affinity[j],
				ContextMatches: sharedTokens(w.glosses[j], own, 12),
			})
		}
		d.Terms = append(d.Terms, td)
	}
	// Words the colony never reached — because the inventory refused partway
	// through building it — still belong in the report. Omitting them makes a
	// truncated analysis look like a complete one over a shorter query, which
	// is how "1 of 1 words resolved" came to be printed for a query of three.
	for _, term := range terms {
		if _, built := c.index[term.POS+":"+term.Lemma]; built {
			continue
		}
		if seenTerm(d.Terms, term) {
			continue
		}
		d.Terms = append(d.Terms, TermDiagnosis{Term: term, ColonyPick: -1, OptimumPick: -1})
	}

	// The decomposition is of the OPTIMUM, not of the colony's answer. When
	// they differ the optimum is the one that needs explaining, and when they
	// agree it makes no difference.
	for i, w := range c.words {
		if optimum[i] < 0 {
			continue
		}
		d.ContextTotal += opts.ContextWeight * w.affinity[optimum[i]]
		for j := i + 1; j < len(c.words); j++ {
			if optimum[j] < 0 {
				continue
			}
			value := c.pairOverlap(i, optimum[i], j, optimum[j])
			d.PairTotal += value
			d.Pairs = append(d.Pairs, PairContribution{
				A: w.term, B: c.words[j].term, Relatedness: value,
				Shared: sharedTokens(w.glosses[optimum[i]], c.words[j].glosses[optimum[j]], 12),
			})
		}
	}
	sort.SliceStable(d.Pairs, func(i, j int) bool { return d.Pairs[i].Relatedness > d.Pairs[j].Relatedness })
	return d, err
}

// ScoreOf evaluates a named assignment — the reading a person expected —
// against the same objective, so "it chose the wrong sense" becomes "it scored
// the right answer 0.12 against 0.22", which is a quantity you can act on.
//
// Senses not named keep whatever the optimum chose, so a partial expectation
// asks the honest question: holding everything else at its best, what does
// insisting on THIS sense cost?
func (d *Diagnosis) ScoreOf(synsetIDs []string) (score float64, matched int) {
	wanted := make(map[string]bool, len(synsetIDs))
	for _, id := range synsetIDs {
		wanted[strings.TrimSpace(id)] = true
	}
	pick := make(assignment, len(d.Terms))
	for i, term := range d.Terms {
		pick[i] = term.OptimumPick
		for j, candidate := range term.Candidates {
			if wanted[candidate.SynsetID] {
				pick[i] = j
				matched++
			}
		}
	}
	return d.colony.objective(pick), matched
}

// enumerate walks every assignment and returns the best.
//
// Deliberately the dumbest possible search: its only job is to be obviously
// correct, so that a disagreement with the colony is evidence about the
// colony rather than about this.
func (c *colony) enumerate(limit int) (best assignment, bestScore float64, space int, exhaustive bool) {
	space = 1
	for _, w := range c.words {
		if n := len(w.candidates); n > 0 {
			space *= n
			if space > limit {
				return nil, 0, space, false
			}
		}
	}
	pick := make(assignment, len(c.words))
	best = make(assignment, len(c.words))
	for i, w := range c.words {
		if len(w.candidates) == 0 {
			pick[i], best[i] = -1, -1
		}
	}
	bestScore = math.Inf(-1)
	var walk func(int)
	walk = func(depth int) {
		if depth == len(c.words) {
			if s := c.objective(pick); s > bestScore {
				bestScore = s
				copy(best, pick)
			}
			return
		}
		if len(c.words[depth].candidates) == 0 {
			walk(depth + 1)
			return
		}
		for i := range c.words[depth].candidates {
			pick[depth] = i
			walk(depth + 1)
		}
	}
	walk(0)
	return best, bestScore, space, true
}

// sharedTokens lists the distinct words two bags have in common. The
// diagnosis's most useful single field: a score is a number, but "these two
// senses agreed because they both contain `he`, `his` and `can`" is a bug
// report.
func sharedTokens(a, b []string, limit int) []string {
	inB := make(map[string]bool, len(b))
	for _, w := range b {
		inB[w] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, w := range a {
		if inB[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	if len(out) > limit && limit > 0 {
		out = out[:limit]
	}
	return out
}

// Report writes the diagnosis as text.
func (d *Diagnosis) Report(w io.Writer, expected []string) {
	fmt.Fprintf(w, "context words: %d\n\n", d.ContextSize)

	fmt.Fprintln(w, "=== CANDIDATES  (aff = agreement with the surrounding text)")
	for _, term := range d.Terms {
		fmt.Fprintf(w, "%s/%s\n", term.Term.Lemma, term.Term.POS)
		if len(term.Candidates) == 0 {
			fmt.Fprintln(w, "    (the inventory has no sense for this word)")
			continue
		}
		for i, candidate := range term.Candidates {
			mark := "  "
			switch {
			case i == term.ColonyPick && i == term.OptimumPick:
				mark = "=>"
			case i == term.ColonyPick:
				mark = "C>"
			case i == term.OptimumPick:
				mark = "O>"
			}
			fmt.Fprintf(w, " %s [%d] %-14s gloss=%-4d aff=%-6.3f %-52.52s  ctx: %s\n",
				mark, i, candidate.SynsetID, candidate.GlossSize, candidate.Affinity,
				candidate.Gloss, strings.Join(candidate.ContextMatches, ","))
		}
	}

	fmt.Fprintf(w, "\n=== SEARCH  (%d assignments, %d of %d words resolved)\n",
		d.SearchSpace, d.Resolved(), len(d.Terms))
	optimal, known := d.SearchIsOptimal()
	switch {
	case d.Resolved() < 2:
		fmt.Fprintf(w, "  THE INVENTORY RESOLVED %d OF %d WORDS. There is no disambiguation\n"+
			"  to judge: check the dictionary is reachable and within its allowance\n"+
			"  before reading anything below as a scoring problem.\n",
			d.Resolved(), len(d.Terms))
	case !known:
		fmt.Fprintf(w, "  search space exceeded the cap; the optimum is unknown.\n"+
			"  colony F = %.4f\n", d.ColonyScore)
	case optimal:
		fmt.Fprintf(w, "  colony F = %.4f == optimum %.4f\n"+
			"  THE SEARCH IS FINE. A wrong sense here is the objective's fault,\n"+
			"  not the colony's — tuning ants, cycles or evaporation cannot help.\n",
			d.ColonyScore, d.OptimumScore)
	default:
		fmt.Fprintf(w, "  colony F = %.4f  <  optimum %.4f\n"+
			"  THE SEARCH IS MISSING THE OPTIMUM. More ants or cycles, or slower\n"+
			"  evaporation, should close this.\n", d.ColonyScore, d.OptimumScore)
	}

	fmt.Fprintln(w, "\n=== WHAT THE SCORE IS MADE OF")
	for _, pair := range d.Pairs {
		fmt.Fprintf(w, "  %-12s x %-12s = %-7.4f  on: %s\n",
			pair.A.Lemma, pair.B.Lemma, pair.Relatedness, strings.Join(pair.Shared, ","))
	}
	total := d.ContextTotal + d.PairTotal
	if total > 0 {
		fmt.Fprintf(w, "  context %.4f (%.0f%%)   pairs %.4f (%.0f%%)\n",
			d.ContextTotal, 100*d.ContextTotal/total, d.PairTotal, 100*d.PairTotal/total)
	}

	if len(expected) > 0 {
		score, matched := d.ScoreOf(expected)
		fmt.Fprintf(w, "\n=== THE READING YOU EXPECTED\n"+
			"  matched %d of %d named senses; it scores %.4f against the optimum's %.4f\n",
			matched, len(expected), score, d.OptimumScore)
		for _, term := range d.Terms {
			for _, candidate := range term.Candidates {
				for _, id := range expected {
					if candidate.SynsetID == strings.TrimSpace(id) {
						fmt.Fprintf(w, "  %-12s %-14s aff=%-6.3f %.60s\n",
							term.Term.Lemma, candidate.SynsetID, candidate.Affinity, candidate.Gloss)
					}
				}
			}
		}
	}
}
