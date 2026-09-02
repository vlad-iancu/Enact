package wsd

import (
	"context"
	"math"
	"math/rand"
)

// ACOOptions tunes the ant colony. The defaults are what ships; every field
// trades quality for time, and none of them costs a single inventory lookup —
// see DisambiguateACO.
type ACOOptions struct {
	Lesk LeskOptions
	// Ants is how many assignments are built per cycle, and Cycles how many
	// rounds of reinforcement follow. Their product is the number of complete
	// assignments considered.
	Ants   int
	Cycles int
	// Evaporation is rho: the share of pheromone lost each cycle. Low values
	// make the colony converge early on whatever it found first, which for a
	// short query is usually the most-frequent-sense assignment it started
	// from — the exact outcome this algorithm exists to escape.
	Evaporation float64
	// Alpha weights accumulated pheromone, Beta the immediate gloss overlap.
	// Beta above Alpha keeps the evidence in charge and the memory advisory.
	Alpha float64
	Beta  float64
	// ContextWeight scales how much a sense's agreement with the surrounding
	// text counts beside its agreement with the other senses chosen. The
	// context is a much larger bag, so without a weight below one it drowns
	// the sense-to-sense signal that is the point of the exercise.
	ContextWeight float64
	// PriorWeight is how strongly sense order — corpus frequency, in a
	// dictionary that provides it — biases the choice. It keeps a word with
	// no evidence either way on the most frequent sense, which is a famously
	// strong baseline, while staying small enough that real overlap evidence
	// overrides it.
	PriorWeight float64
	// Seed fixes the pseudo-random stream. Fixed rather than time-based on
	// purpose: two runs of the same crawl must produce the same report, or a
	// user comparing this week's report with last week's cannot tell a
	// changed corpus from a different roll of the dice.
	Seed int64
}

// DefaultACOOptions are the shipped bounds.
var DefaultACOOptions = ACOOptions{
	Lesk:          DefaultLeskOptions,
	Ants:          20,
	Cycles:        30,
	Evaporation:   0.35,
	Alpha:         1,
	Beta:          2,
	ContextWeight: 0.35,
	PriorWeight:   0.6,
	Seed:          1,
}

func (o ACOOptions) withDefaults() ACOOptions {
	d := DefaultACOOptions
	if o.Ants <= 0 {
		o.Ants = d.Ants
	}
	if o.Cycles <= 0 {
		o.Cycles = d.Cycles
	}
	if o.Evaporation <= 0 || o.Evaporation >= 1 {
		o.Evaporation = d.Evaporation
	}
	if o.Alpha <= 0 {
		o.Alpha = d.Alpha
	}
	if o.Beta <= 0 {
		o.Beta = d.Beta
	}
	if o.ContextWeight <= 0 {
		o.ContextWeight = d.ContextWeight
	}
	if o.PriorWeight <= 0 {
		o.PriorWeight = d.PriorWeight
	}
	if o.Lesk.MaxSenses <= 0 {
		o.Lesk.MaxSenses = d.Lesk.MaxSenses
	}
	if o.Lesk.MaxRelated <= 0 {
		o.Lesk.MaxRelated = d.Lesk.MaxRelated
	}
	if o.Seed == 0 {
		o.Seed = d.Seed
	}
	return o
}

// DisambiguateACO assigns senses to a whole text at once, by ant colony
// optimisation over the Lesk objective.
//
// Why this exists. Plain extended Lesk decides each word by itself: it scores
// every candidate sense against the surrounding words and keeps the best. That
// is a local decision, and it has no way to notice that its answers contradict
// each other. Measured on the query "opensearch indices, security, syntax and
// usage", it chose the software sense of `opensearch`, the SEMIOTICS sense of
// `index`, the COLLATERAL sense of `security` and the HABIT sense of `usage` —
// four senses that never co-occur in any text ever written. Each was
// defensible alone. Together they are nonsense, and the crawl went looking for
// philosophy.
//
// What is optimised. A whole assignment at once, scored by how much its senses
// agree with each other and with the surrounding text:
//
//	F(assignment) = sum over pairs i<j of overlap(sense_i, sense_j)
//	                + ContextWeight * sum over i of overlap(sense_i, context)
//
// Maximising that directly is exponential — the product of every word's sense
// count, which for a twenty-word query with ten senses each is 10^20. So it is
// searched rather than solved, and ant colony optimisation fits because the
// problem decomposes the way ACO expects: an assignment is built one word at a
// time, and a partial assignment already says which senses of the next word
// would fit.
//
// How. Each ant builds one complete assignment, choosing word by word in a
// shuffled order, preferring senses that agree with what it has already
// chosen. Good assignments deposit pheromone on the (word, sense) pairs they
// used, so later ants are drawn towards senses that have been part of coherent
// assignments before; evaporation stops the first lucky assignment from
// locking the colony in. What survives is a set of senses that support one
// another, which is what "understanding the query" means here.
//
// It costs no extra inventory lookups. Every candidate sense and its extended
// gloss is fetched exactly once, before the colony starts — the same fetches
// plain Lesk would make. Everything after that is arithmetic over those
// glosses, so a metered dictionary sees no difference between this and the
// greedy version. That is what makes it affordable on the query.
//
// contextBag is the surrounding text: the query's own lemmas, and — for a
// crawl — the salient lemmas of the pages it was told to start from, which is
// usually far more signal than the query alone carries.
func DisambiguateACO(ctx context.Context, inv Inventory, tax *Taxonomy,
	terms []Term, contextBag []string, opts ACOOptions) ([]Sense, error) {

	opts = opts.withDefaults()
	if len(terms) == 0 {
		return nil, nil
	}
	c, err := newColony(ctx, inv, tax, terms, contextBag, opts)
	if c == nil {
		return nil, err
	}
	c.seal()
	// Candidates resolved before an exhausted allowance are still worth
	// running the colony over: a reading of half the query beats none, and the
	// caller reports the error alongside it either way.
	return c.senses(c.run()), err
}

// word is one distinct lemma of the text and the senses it might carry.
type word struct {
	key        string
	term       Term
	candidates []Synset
	// glosses[i] is candidate i's extended gloss, computed once.
	glosses [][]string
	// affinity[i] is candidate i's overlap with the surrounding text. It does
	// not change as the assignment does, so it is computed once.
	affinity []float64
	// prior[i] falls with sense order, standing in for corpus frequency.
	prior []float64
	// pheromone[i] is what the colony has learned about candidate i.
	pheromone []float64
}

// assignment maps each word to the index of its chosen candidate, or -1.
// An int slice rather than a map keyed by lemma because every ant of every
// cycle builds one and reads it once per candidate it considers; this is the
// hot path.
type assignment []int

type colony struct {
	opts  ACOOptions
	words []*word
	// terms is the original sequence, so the result comes back in the order
	// the text used rather than the order the colony worked in.
	terms []Term
	// index maps a term key to its position in words, since a repeated word
	// is one decision, not several.
	index map[string]int
	// pairs memoises overlap between two candidates, keyed by their global
	// numbers. The colony revisits the same pair thousands of times and the
	// overlap is the expensive part of the whole algorithm.
	pairs map[[2]int]float64
	// offsets[w] is where word w's candidates start in the global numbering.
	offsets []int
	rng     *rand.Rand
}

// newColony resolves every candidate sense and its extended gloss. This is the
// only part that touches the inventory.
func newColony(ctx context.Context, inv Inventory, tax *Taxonomy,
	terms []Term, contextBag []string, opts ACOOptions) (*colony, error) {

	c := &colony{
		opts: opts, terms: terms,
		index: map[string]int{},
		pairs: map[[2]int]float64{},
		rng:   rand.New(rand.NewSource(opts.Seed)),
	}
	for _, term := range terms {
		key := term.POS + ":" + term.Lemma
		if _, seen := c.index[key]; seen {
			continue
		}
		w := &word{key: key, term: term}
		c.index[key] = len(c.words)
		c.words = append(c.words, w)

		// A word is not evidence about itself: every one of its senses lists
		// it as a lemma, so leaving it in the context scores a point for all
		// of them equally and discriminates nothing.
		own := withoutLemma(contextBag, term.Lemma)

		candidates, err := inv.Senses(ctx, term.Lemma, term.POS)
		if err != nil {
			return c, err
		}
		if len(candidates) > opts.Lesk.MaxSenses {
			candidates = candidates[:opts.Lesk.MaxSenses]
		}
		for rank, candidate := range candidates {
			gloss, err := extendedGloss(ctx, inv, tax, candidate, opts.Lesk.MaxRelated)
			if err != nil {
				return c, err
			}
			w.candidates = append(w.candidates, candidate)
			w.glosses = append(w.glosses, gloss)
			w.affinity = append(w.affinity, relatedness(gloss, own))
			// Rank 0 is the dictionary's own first sense. The decay is gentle
			// so it orders equals rather than deciding anything.
			w.prior = append(w.prior, 1/(1+float64(rank)))
			// Every candidate starts equal. Seeding pheromone with the prior
			// would hand the first cycle to the most frequent sense, and the
			// evaporation schedule would never fully undo it.
			w.pheromone = append(w.pheromone, 1)
		}
	}
	return c, nil
}

// seal fixes the global candidate numbering the pair memo is keyed by.
//
// Separate from construction because the numbering shifts every time a word is
// added, so a memo filled in while words were still arriving would be keyed on
// numbers that no longer mean the same thing.
func (c *colony) seal() {
	c.offsets = make([]int, len(c.words))
	next := 0
	for i, w := range c.words {
		c.offsets[i] = next
		next += len(w.candidates)
	}
}

// run is the colony proper: build, score, reinforce, evaporate.
func (c *colony) run() assignment {
	if len(c.words) == 0 {
		return nil
	}
	var best assignment
	bestScore := math.Inf(-1)

	order := make([]int, len(c.words))
	for i := range order {
		order[i] = i
	}

	for cycle := 0; cycle < c.opts.Cycles; cycle++ {
		var cycleBest assignment
		cycleScore := math.Inf(-1)
		for ant := 0; ant < c.opts.Ants; ant++ {
			// A fresh order per ant. Whichever word is decided first is
			// decided on the context alone, so a fixed order would make that
			// one word's greedy choice a permanent bias for every ant.
			c.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			candidate := c.walk(order)
			if score := c.objective(candidate); score > cycleScore {
				cycleBest, cycleScore = candidate, score
			}
		}
		if cycleScore > bestScore {
			best, bestScore = cycleBest, cycleScore
		}
		c.evaporate()
		// Only the best assignments reinforce. Letting every ant deposit makes
		// pheromone track how often a sense was tried rather than how well it
		// did, and the colony stops discriminating.
		c.deposit(cycleBest, cycleScore)
		c.deposit(best, bestScore)
	}
	return best
}

// walk builds one assignment, one word at a time.
func (c *colony) walk(order []int) assignment {
	choice := make(assignment, len(c.words))
	for i := range choice {
		choice[i] = -1
	}
	for _, w := range order {
		word := c.words[w]
		if len(word.candidates) == 0 {
			continue
		}
		weights := make([]float64, len(word.candidates))
		var total float64
		for i := range word.candidates {
			// Desirability: agreement with the senses already chosen, plus the
			// standing agreement with the text, plus the frequency prior. One
			// is added so a candidate with no overlap at all still has a
			// chance proportional to its pheromone — an ant that could never
			// choose it could never discover it was right.
			fit := 1 + c.agreement(w, i, choice) +
				c.opts.ContextWeight*word.affinity[i] +
				c.opts.PriorWeight*word.prior[i]
			weights[i] = math.Pow(word.pheromone[i], c.opts.Alpha) *
				math.Pow(fit, c.opts.Beta)
			total += weights[i]
		}
		choice[w] = c.roulette(weights, total)
	}
	return choice
}

// agreement is how well candidate i of word w fits what has been chosen.
func (c *colony) agreement(w, i int, choice assignment) float64 {
	var sum float64
	for other, pick := range choice {
		if pick >= 0 {
			sum += c.pairOverlap(w, i, other, pick)
		}
	}
	return sum
}

// pairOverlap is the Lesk overlap of two candidates, memoised.
func (c *colony) pairOverlap(w1, i1, w2, i2 int) float64 {
	if w1 == w2 {
		return 0
	}
	a, b := c.offsets[w1]+i1, c.offsets[w2]+i2
	if a > b {
		a, b = b, a
	}
	key := [2]int{a, b}
	if v, ok := c.pairs[key]; ok {
		return v
	}
	v := relatedness(c.words[w1].glosses[i1], c.words[w2].glosses[i2])
	c.pairs[key] = v
	return v
}

// objective scores a complete assignment: the quantity the colony maximises.
func (c *colony) objective(choice assignment) float64 {
	var sum float64
	for i, w := range c.words {
		pick := choice[i]
		if pick < 0 {
			continue
		}
		sum += c.opts.ContextWeight * w.affinity[pick]
		for j := i + 1; j < len(c.words); j++ {
			if p2 := choice[j]; p2 >= 0 {
				sum += c.pairOverlap(i, pick, j, p2)
			}
		}
	}
	return sum
}

func (c *colony) evaporate() {
	for _, w := range c.words {
		for i := range w.pheromone {
			w.pheromone[i] *= 1 - c.opts.Evaporation
			// A floor, so a sense that loses early can still be reconsidered.
			// Without it pheromone decays towards zero and the choice becomes
			// irreversible, which is premature convergence — the
			// characteristic way an ant colony fails.
			if w.pheromone[i] < 0.01 {
				w.pheromone[i] = 0.01
			}
		}
	}
}

func (c *colony) deposit(choice assignment, score float64) {
	if choice == nil || score <= 0 || math.IsInf(score, 0) {
		return
	}
	// Divided by the word count so that a long text, which has more pairs and
	// therefore a larger objective, does not deposit so much that the first
	// good assignment saturates every trail.
	share := score / float64(len(c.words))
	for i, w := range c.words {
		if pick := choice[i]; pick >= 0 && pick < len(w.pheromone) {
			w.pheromone[pick] += share
		}
	}
}

// roulette picks an index with probability proportional to its weight.
func (c *colony) roulette(weights []float64, total float64) int {
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return 0
	}
	r := c.rng.Float64() * total
	for i, w := range weights {
		if math.IsNaN(w) || math.IsInf(w, 0) {
			continue
		}
		r -= w
		if r <= 0 {
			return i
		}
	}
	return len(weights) - 1
}

// senses turns the winning assignment into the report, in the text's own word
// order and with a repeated word sharing one decision.
func (c *colony) senses(choice assignment) []Sense {
	out := make([]Sense, 0, len(c.terms))
	for _, term := range c.terms {
		sense := Sense{Term: term}
		w, ok := c.index[term.POS+":"+term.Lemma]
		if !ok || len(c.words[w].candidates) == 0 {
			out = append(out, sense)
			continue
		}
		word := c.words[w]
		sense.Candidates = make([]string, 0, len(word.candidates))
		for _, candidate := range word.candidates {
			sense.Candidates = append(sense.Candidates, candidate.ID)
		}
		// Falling back to the dictionary's first sense covers the word the
		// colony never got to, which happens when the inventory ran out
		// partway through building it.
		pick := 0
		if w < len(choice) && choice[w] >= 0 && choice[w] < len(word.candidates) {
			pick = choice[w]
		}
		chosen := word.candidates[pick]
		sense.SynsetID = chosen.ID
		sense.Gloss = chosen.Gloss
		sense.WordNetKey = chosen.WordNetKey
		// The reported score is this sense's own contribution to the winning
		// assignment — how much it agreed with the senses chosen around it.
		// Not comparable with plain Lesk's number, and more useful: it says
		// whether a word was PART of the reading or merely along for the ride.
		sense.Score = c.contribution(w, pick, choice)
		out = append(out, sense)
	}
	return out
}

func (c *colony) contribution(w, pick int, choice assignment) float64 {
	score := c.opts.ContextWeight * c.words[w].affinity[pick]
	for other, p := range choice {
		if other != w && p >= 0 {
			score += c.pairOverlap(w, pick, other, p)
		}
	}
	return score
}

// SalientLemmas reduces a body of text to the lemmas worth using as context,
// most characteristic first.
//
// Seed pages run to thousands of words, and the colony compares every context
// word against every candidate gloss, so the whole page is both unaffordable
// and worse: furniture words appear in every gloss and would swamp the
// evidence that distinguishes one sense from another.
func SalientLemmas(terms []Term, idf func(string) float64, n int) []string {
	salient := Salient(terms, idf, n)
	out := make([]string, 0, len(salient))
	seen := map[string]bool{}
	for _, t := range salient {
		if seen[t.Lemma] {
			continue
		}
		seen[t.Lemma] = true
		out = append(out, t.Lemma)
	}
	return out
}

// relatedness is overlap normalised for how much text each sense brings.
//
// Raw Banerjee-Pedersen overlap is an absolute count, which is fine when it
// compares one candidate against a fixed context but wrong when it compares
// candidates against EACH OTHER: a sense with many hyponyms accumulates a much
// larger extended gloss and therefore out-overlaps everything, whatever it
// means. Measured on "security", the winning sense carried 135 tokens and the
// sense a person would choose carried 18 — a seven-fold handicap that no
// amount of actual relevance could close.
//
// Dividing by the geometric mean of the two bag sizes is the standard remedy
// and the symmetric one: it asks what SHARE of the available text is shared,
// so a short precise gloss can beat a long vague one. The squared-run
// weighting inside overlap survives it untouched, because the numerator is
// still Banerjee and Pedersen's.
//
// Left out of overlap() itself deliberately. Greedy Lesk compares candidates
// of ONE word against ONE context bag, where the divisor is nearly constant
// and normalising would only add noise; and its behaviour is pinned by tests
// that describe the published algorithm.
func relatedness(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return overlap(a, b) / math.Sqrt(float64(len(a))*float64(len(b)))
}
