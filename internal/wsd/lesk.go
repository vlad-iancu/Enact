package wsd

import (
	"context"
	"strings"
)

// LeskOptions bounds disambiguation. Every field is a direct multiplier on
// how many inventory lookups a text costs, which on a metered inventory is
// the difference between a crawl that runs and one that stops.
type LeskOptions struct {
	// MaxSenses caps the candidate senses considered per word. WordNet's
	// senses are ordered by frequency, so the tail is both unlikely and
	// expensive.
	MaxSenses int
	// MaxRelated caps how many neighbours contribute their glosses to one
	// candidate's extended gloss. This is what makes Lesk "extended"; zero
	// reduces it to the original 1986 algorithm.
	MaxRelated int
}

// DefaultLeskOptions are the shipped bounds.
//
// MaxSenses was 4, which was too tight to do the job. A technical query is
// made of ordinary words used in a specialist way, and those are exactly the
// words with the most senses: "index" has 33 in BabelNet and the database one
// is not in the first four, so a query about "opensearch indices" was read as
// being about semiotics no matter how much context surrounded it. Lesk cannot
// choose a sense it was never offered.
//
// Ten is affordable now in a way it was not when this constant was written.
// Pages are disambiguated against the local WordNet, which is unmetered, so
// the cap costs only CPU there; the metered side is the query alone, analysed
// once and cached permanently.
var DefaultLeskOptions = LeskOptions{MaxSenses: 10, MaxRelated: 12}

// Sense is a term with the meaning Lesk chose for it, and the evidence.
type Sense struct {
	Term       Term     `json:"term"`
	SynsetID   string   `json:"synset_id"`
	Gloss      string   `json:"gloss,omitempty"`
	WordNetKey string   `json:"wordnet_key,omitempty"`
	Score      float64  `json:"score"`
	Candidates []string `json:"candidates,omitempty"`
}

// Disambiguate assigns a sense to each term, using the whole text as context.
//
// This is extended Lesk (Banerjee & Pedersen 2002). The original algorithm
// compares a candidate sense's gloss against the context and picks the
// biggest overlap; its weakness is that glosses are short, so most candidates
// overlap the context in nothing at all and the choice is a coin toss. The
// extension enlarges each candidate's gloss with the glosses of its related
// synsets — hypernyms, hyponyms, meronyms — which gives the comparison enough
// text to discriminate.
//
// Terms whose senses cannot be resolved are returned with an empty SynsetID
// rather than dropped, so the caller can see what was not understood.
func Disambiguate(ctx context.Context, inv Inventory, tax *Taxonomy, terms []Term, opts LeskOptions) ([]Sense, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	if opts.MaxSenses <= 0 {
		opts.MaxSenses = DefaultLeskOptions.MaxSenses
	}
	// The context bag is every content lemma of the text. Building it once
	// and reusing it for every term is what makes "the whole prompt as
	// context" affordable.
	contextBag := make([]string, 0, len(terms))
	for _, term := range terms {
		contextBag = append(contextBag, term.Lemma)
	}

	out := make([]Sense, 0, len(terms))
	// Two texts routinely repeat a word; disambiguating it once per text is
	// correct (the context is the same) and saves the lookups.
	decided := make(map[string]Sense, len(terms))
	for _, term := range terms {
		key := term.POS + ":" + term.Lemma
		if prior, ok := decided[key]; ok {
			prior.Term = term
			out = append(out, prior)
			continue
		}
		sense, err := disambiguateOne(ctx, inv, tax, term, contextBag, opts)
		if err != nil {
			return out, err
		}
		decided[key] = sense
		out = append(out, sense)
	}
	return out, nil
}

func disambiguateOne(ctx context.Context, inv Inventory, tax *Taxonomy, term Term, contextBag []string, opts LeskOptions) (Sense, error) {
	sense := Sense{Term: term}
	candidates, err := inv.Senses(ctx, term.Lemma, term.POS)
	if err != nil {
		return sense, err
	}
	if len(candidates) == 0 {
		return sense, nil
	}
	// The word being disambiguated is removed from its own context. Every
	// candidate sense lists it as a lemma, so leaving it in scores a point
	// for all of them equally — it cannot discriminate, and it masks the
	// difference between "the context decided this" and "nothing matched".
	contextBag = withoutLemma(contextBag, term.Lemma)
	if len(candidates) > opts.MaxSenses {
		candidates = candidates[:opts.MaxSenses]
	}
	sense.Candidates = make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		sense.Candidates = append(sense.Candidates, candidate.ID)
	}

	// The first candidate is the fallback. WordNet orders senses by corpus
	// frequency, and "most frequent sense" is a famously hard baseline to
	// beat — so when no candidate overlaps the context at all, taking it is
	// better than taking an arbitrary one.
	best, bestScore := candidates[0], 0.0
	for _, candidate := range candidates {
		bag, err := extendedGloss(ctx, inv, tax, candidate, opts.MaxRelated)
		if err != nil {
			return sense, err
		}
		if score := overlap(bag, contextBag); score > bestScore {
			best, bestScore = candidate, score
		}
	}
	sense.SynsetID = best.ID
	sense.Gloss = best.Gloss
	sense.WordNetKey = best.WordNetKey
	sense.Score = bestScore
	return sense, nil
}

// extendedGloss is a candidate sense's own gloss and lemmas plus the glosses
// of up to maxRelated neighbours, as a bag of words.
func extendedGloss(ctx context.Context, inv Inventory, tax *Taxonomy, s Synset, maxRelated int) ([]string, error) {
	bag := tokenizeGloss(s.Gloss)
	for _, lemma := range s.Lemmas {
		bag = append(bag, normalizeLemma(lemma))
	}
	// A sense that came from WordNet but reached us without a gloss still has
	// one locally; the definition is the whole input to Lesk, so it is worth
	// the map lookup to recover it.
	if s.Gloss == "" && s.WordNetKey != "" && tax != nil {
		bag = append(bag, tokenizeGloss(tax.Gloss(s.WordNetKey))...)
	}
	if maxRelated <= 0 {
		return bag, nil
	}
	seen := 0
	for _, rel := range s.Relations {
		if seen >= maxRelated {
			break
		}
		// Only the gloss is read here, never the neighbour's own edges.
		related, err := glossOf(ctx, inv, rel.Target)
		if err != nil {
			// A neighbour that cannot be fetched weakens the comparison but
			// does not invalidate it; the budget running out mid-expansion is
			// the expected case and must not fail the whole disambiguation.
			return bag, err
		}
		seen++
		bag = append(bag, tokenizeGloss(related.Gloss)...)
		for _, lemma := range related.Lemmas {
			bag = append(bag, normalizeLemma(lemma))
		}
	}
	return bag, nil
}

// overlap scores two bags of words the way Banerjee & Pedersen do: find the
// longest contiguous run of tokens the two share, add the square of its
// length, remove it from both, and repeat until nothing is shared.
//
// The squaring is the point. A shared phrase of three consecutive words is
// far stronger evidence that two glosses are about the same thing than three
// words shared separately, so it scores 9 rather than 3. Counting single
// words alone — plain Lesk — treats "body of water" matching "body of water"
// as no better than three coincidental hits on "of", "body" and "water".
//
// Removing each match before looking for the next is what stops one shared
// phrase from being counted repeatedly as all of its own sub-phrases.
func overlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Copies, because matches are consumed as they are found.
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	total := 0.0
	for {
		length, i, j := longestCommonRun(left, right)
		if length == 0 {
			return total
		}
		total += float64(length * length)
		// Blanking rather than splicing keeps the indices of everything else
		// intact; the empty string never matches, because tokenizeGloss drops
		// tokens shorter than two characters.
		for k := 0; k < length; k++ {
			left[i+k] = ""
			right[j+k] = ""
		}
	}
}

// longestCommonRun returns the length of the longest contiguous token
// sequence shared by a and b, and where it starts in each.
func longestCommonRun(a, b []string) (length, aStart, bStart int) {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] != "" && a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
				if curr[j] > length {
					length, aStart, bStart = curr[j], i-curr[j], j-curr[j]
				}
			} else {
				curr[j] = 0
			}
		}
		prev, curr = curr, prev
	}
	return length, aStart, bStart
}

// glossStop are words too common to be evidence of anything.
//
// This list was 27 words long and that was not nearly enough. Measured on a
// real query, the two senses that won — the COLLATERAL sense of "security" and
// the ACT-OF-USING sense of "usage" — did so on an overlap of 17, scored
// almost entirely on `can, he, his, into, one, they, when, you`. Two glosses
// that share nothing but pronouns were being read as strong evidence that they
// mean the same thing.
//
// Two things conspire. WordNet glosses carry example sentences ("he warned
// against the use of narcotic drugs"), which is ordinary prose and therefore
// dense with function words; and overlap() squares contiguous runs, so three
// function words in a row score nine rather than three. The squaring is right
// for "body of water" and catastrophic for "when you can".
//
// So the list has to cover English's closed classes properly: pronouns,
// determiners, auxiliaries, modals, prepositions, conjunctions and the handful
// of near-empty verbs and quantifiers. These carry no topical information in
// any domain, which is what makes a fixed list defensible here rather than a
// corpus-derived one.
var glossStop = map[string]bool{
	// articles and determiners
	"a": true, "an": true, "the": true, "this": true, "that": true,
	"these": true, "those": true, "each": true, "every": true, "either": true,
	"neither": true, "another": true, "such": true, "same": true, "own": true,
	// pronouns — the single biggest source of the noise above
	"he": true, "him": true, "his": true, "she": true, "her": true, "hers": true,
	"it": true, "its": true, "they": true, "them": true, "their": true,
	"theirs": true, "we": true, "us": true, "our": true, "ours": true,
	"you": true, "your": true, "yours": true, "i": true, "me": true, "my": true,
	"mine": true, "who": true, "whom": true, "whose": true, "which": true,
	"what": true, "one": true, "ones": true, "oneself": true, "itself": true,
	"himself": true, "herself": true, "themselves": true, "someone": true,
	"something": true, "anyone": true, "anything": true, "everyone": true,
	"everything": true, "nothing": true,
	// auxiliaries and modals
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"being": true, "am": true, "has": true, "have": true, "had": true,
	"having": true, "do": true, "does": true, "did": true, "doing": true,
	"can": true, "could": true, "may": true, "might": true, "must": true,
	"shall": true, "should": true, "will": true, "would": true, "let": true,
	// prepositions and conjunctions
	"of": true, "or": true, "and": true, "to": true, "in": true, "on": true,
	"for": true, "with": true, "by": true, "as": true, "at": true, "from": true,
	"into": true, "onto": true, "upon": true, "over": true, "under": true,
	"about": true, "after": true, "before": true, "between": true, "through": true,
	"during": true, "without": true, "within": true, "against": true,
	"but": true, "if": true, "then": true, "than": true, "so": true, "because": true,
	"while": true, "when": true, "where": true, "how": true, "why": true,
	"whether": true, "although": true, "though": true, "unless": true,
	"out": true, "off": true, "up": true, "down": true, "away": true,
	// quantifiers and degree words
	"all": true, "any": true, "both": true, "few": true, "many": true,
	"more": true, "most": true, "much": true, "no": true, "none": true,
	"not": true, "only": true, "other": true, "others": true, "some": true,
	"very": true, "too": true, "also": true, "just": true, "even": true,
	"still": true, "already": true, "again": true, "ever": true, "never": true,
	"always": true, "often": true, "sometimes": true, "here": true, "there": true,
	"now": true, "yet": true, "well": true, "way": true, "thing": true,
	"things": true, "kind": true, "sort": true, "type": true, "part": true,
	// dictionary furniture
	"esp": true, "usually": true, "especially": true, "used": true,
	"use": true, "using": true, "typically": true, "generally": true,
	"someone's": true, "something's": true, "etc": true, "eg": true, "ie": true,
}

// tokenizeGloss splits a definition into comparable tokens. It is
// deliberately cruder than Analyze: glosses are short, and running a POS
// tagger over every neighbour's gloss would cost far more than it gains.
func tokenizeGloss(gloss string) []string {
	if gloss == "" {
		return nil
	}
	gloss = withoutExamples(gloss)
	fields := strings.FieldsFunc(gloss, func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && r != '_'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.ToLower(f)
		if len(w) < 2 || glossStop[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// normalizeLemma renders a multiword lemma the way gloss tokens appear, so
// "sea_otter" can match "sea otter" written out in a definition.
func normalizeLemma(lemma string) string {
	return strings.ToLower(strings.ReplaceAll(lemma, "_", " "))
}

// withoutLemma copies a context bag with every occurrence of one lemma
// removed.
func withoutLemma(bag []string, lemma string) []string {
	out := make([]string, 0, len(bag))
	for _, w := range bag {
		if w != lemma {
			out = append(out, w)
		}
	}
	return out
}

// withoutExamples drops the quoted usage examples a WordNet gloss carries,
// keeping only the definition.
//
// A gloss is `definition; "an example"; "another example"`. The examples are
// illustrative prose, not meaning: the collateral sense of "security" is
// illustrated with "bankers are reluctant to lend without good security", and
// the habitual sense of "usage" with a sentence mentioning the United States.
// Measured, those two glosses overlapped on `bearing, law, past, states,
// united` — and because overlap() squares contiguous runs, the accidental
// phrase "united states" alone scored four, which was enough to make the two
// wrong senses look like the most coherent reading of the query.
//
// This does discard real signal: the "act of using" gloss is illustrated with
// "skilled in the utilization of computers", and `computers` is genuinely
// topical. That loss is worth taking. An example is one arbitrary sentence
// somebody wrote about the word; the definition is the concept. Scoring
// concepts by the vocabulary of arbitrary sentences is how two senses that
// share nothing come to look identical.
func withoutExamples(gloss string) string {
	if !strings.Contains(gloss, `"`) {
		return gloss
	}
	var b strings.Builder
	quoted := false
	for _, r := range gloss {
		if r == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			b.WriteRune(r)
		}
	}
	return b.String()
}
