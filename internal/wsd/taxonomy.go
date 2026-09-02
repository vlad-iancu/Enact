package wsd

import (
	"fmt"
	"strings"

	"github.com/fluhus/gostuff/nlp/wordnet"
)

// Taxonomy is the local WordNet 3.0 database: the IS-A hierarchy that
// concept-to-concept similarity is measured on, and the morphology that
// lemmatisation uses.
//
// It is separate from Inventory on purpose. The inventory answers "what can
// this word mean", and BabelNet answers that far better than WordNet because
// it also knows Wikipedia. But similarity needs a clean, connected IS-A tree
// with meaningful depths, which is what WordNet is and what BabelNet's merged
// graph is not. Every BabelNet sense that came from WordNet carries its
// original offset, so the two fit together: BabelNet decides what a word
// means, WordNet measures how close two meanings are.
type Taxonomy struct {
	wn *wordnet.WordNet
}

// Config locates the WordNet database.
type Config struct {
	// WordNetDir is the WordNet 3.0 `dict` directory. Fetched by
	// `make wordnet`; there is no default because a wrong guess would fail
	// at the first similarity computation rather than at startup.
	WordNetDir string `env:"WORDNET_DIR"`
}

// NewTaxonomy parses the WordNet database. It costs a few hundred
// milliseconds and a little over a hundred megabytes of heap, so it is done
// once at startup and shared.
func NewTaxonomy(cfg Config) (*Taxonomy, error) {
	if strings.TrimSpace(cfg.WordNetDir) == "" {
		return nil, fmt.Errorf("wsd: WORDNET_DIR is not set; run `make wordnet` and point it at dist/wordnet/WordNet-3.0/dict")
	}
	wn, err := wordnet.Parse(cfg.WordNetDir)
	if err != nil {
		return nil, fmt.Errorf("wsd: parse wordnet at %q: %w", cfg.WordNetDir, err)
	}
	return &Taxonomy{wn: wn}, nil
}

// Size reports how many synsets were loaded, for a startup log line that
// proves the database is really there.
func (t *Taxonomy) Size() int { return len(t.wn.Synset) }

// NormalizeWordNetKey converts BabelNet's WordNet reference ("wn:02084071n")
// into the local database's key ("n02084071").
//
// The two notations differ only in where the part of speech sits, but they
// differ every time, so this is the single place that knows it. An input that
// is not a WordNet reference yields "" rather than a wrong key.
func NormalizeWordNetKey(ref string) string {
	s := strings.TrimSpace(ref)
	s = strings.TrimPrefix(s, "wn:")
	if len(s) < 2 {
		return ""
	}
	// Already local notation: a leading part-of-speech letter.
	if isPOSLetter(s[:1]) && allDigits(s[1:]) {
		return s
	}
	pos, offset := s[len(s)-1:], s[:len(s)-1]
	if !isPOSLetter(pos) || !allDigits(offset) {
		return ""
	}
	// WordNet's adjective satellites ('s') live in the adjective files.
	if pos == "s" {
		pos = POSAdjective
	}
	return pos + offset
}

func isPOSLetter(s string) bool {
	switch s {
	case POSNoun, POSVerb, POSAdjective, POSAdverb, "s":
		return true
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Similarity returns the Wu-Palmer similarity of two WordNet synsets, in
// [0,1], where 1 means the same synset.
//
// Wu-Palmer scores two concepts by the depth of their least common subsumer
// against their own depths, so it rewards a shared ancestor that is itself
// specific. `dog` and `cat` share `carnivore` and score highly; `dog` and
// `bank` share only `entity` and score low.
//
// A key this database does not have — a BabelNet-only sense, typically a
// named entity — scores 0. That is deliberately not an error: a page full of
// proper nouns is simply a page this measure has little to say about, and the
// BM25 half of the score is what carries it.
func (t *Taxonomy) Similarity(aKey, bKey string) float64 {
	if aKey == "" || bKey == "" {
		return 0
	}
	if aKey == bKey {
		return 1
	}
	a, aok := t.wn.Synset[aKey]
	b, bok := t.wn.Synset[bKey]
	if !aok || !bok {
		return 0
	}
	// Wu-Palmer is only defined within one hierarchy; nouns and verbs are
	// separate trees with no shared root.
	if a.Pos != b.Pos {
		return 0
	}
	// simulateRoot joins the otherwise disconnected verb hierarchies under a
	// fake root, so two unrelated verbs score low rather than zero.
	return t.wn.WupSimilarity(a, b, true)
}

// Gloss returns a WordNet synset's definition, for inventories that have an
// offset but no gloss of their own.
func (t *Taxonomy) Gloss(key string) string {
	if ss, ok := t.wn.Synset[key]; ok {
		return ss.Gloss
	}
	return ""
}

// morphyRules are WordNet's suffix detachment rules, per part of speech, in
// the order the original Morphy applies them.
var morphyRules = map[string][][2]string{
	POSNoun: {
		{"ses", "s"}, {"xes", "x"}, {"zes", "z"}, {"ches", "ch"}, {"shes", "sh"},
		{"men", "man"}, {"ies", "y"}, {"s", ""},
	},
	POSVerb: {
		{"ies", "y"}, {"es", "e"}, {"es", ""}, {"ed", "e"}, {"ed", ""},
		{"ing", "e"}, {"ing", ""}, {"s", ""},
	},
	POSAdjective: {
		{"er", ""}, {"est", ""}, {"er", "e"}, {"est", "e"},
	},
}

// Lemmatize reduces an inflected form to its dictionary form, following
// WordNet's Morphy: consult the exception list, then try suffix detachment,
// and accept a base form only if the database actually knows it.
//
// That last condition is what keeps it honest. Detaching "s" from "bus"
// yields "bu", which is not a word — Morphy avoids that not by knowing about
// "bus" but by refusing a base form the dictionary has never heard of.
//
// Two traps this has to sidestep, both found by its tests:
//
//   - Returning early when the input is itself a known lemma is wrong.
//     WordNet lists "banks" (the surname) and "rates", so "banks" would never
//     reach the rule that produces "bank". Inflected forms are frequently
//     also rare lemmas.
//   - Preferring the input over the exception list is wrong for the same
//     reason: "better" is a known adjective, so it would never reach the
//     entry that maps it to "good".
//
// The tiebreak, when both the input and a rule-derived form are known, is
// polysemy: a genuine lemma carries the word's many senses, while an
// accidental collision like "Banks" carries one. That keeps "species" from
// being reduced to "specie".
func (t *Taxonomy) Lemmatize(word, pos string) string {
	w := strings.ToLower(strings.TrimSpace(word))
	if w == "" {
		return ""
	}
	// The exception list is authoritative: it exists precisely for the forms
	// no rule can derive.
	if forms := t.wn.Exception[pos+"."+w]; len(forms) > 0 {
		base := strings.TrimPrefix(forms[0], pos+".")
		if t.senses(base, pos) > 0 {
			return base
		}
	}
	own := t.senses(w, pos)
	for _, rule := range morphyRules[pos] {
		suffix, replacement := rule[0], rule[1]
		if !strings.HasSuffix(w, suffix) {
			continue
		}
		candidate := strings.TrimSuffix(w, suffix) + replacement
		if candidate == "" || candidate == w {
			continue
		}
		if derived := t.senses(candidate, pos); derived > 0 && derived >= own {
			return candidate
		}
	}
	return w
}

// senses counts the synsets a lemma appears in under a part of speech; zero
// means the database does not list it.
func (t *Taxonomy) senses(lemma, pos string) int {
	return len(t.wn.Lemma[pos+strings.ReplaceAll(lemma, " ", "_")])
}
