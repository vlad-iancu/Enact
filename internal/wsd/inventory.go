// Package wsd implements the knowledge-based relevance function behind
// focused crawling: it turns text into word senses (synsets) rather than
// words, and compares texts by how close their senses are in a semantic
// network.
//
// The pipeline, and the papers it follows:
//
//  1. Tokenize and POS-tag, keep content words, lemmatise (tokenize.go).
//  2. Disambiguate each with EXTENDED Lesk — overlaps counted against the
//     glosses of related synsets too, not just the sense's own gloss
//     (Banerjee & Pedersen 2002; lesk.go).
//  3. Expand the resulting senses across the semantic graph with decaying
//     weights (expand.go).
//  4. Score a document against the query with the Mihalcea, Corley &
//     Strapparava (2006) text-to-text measure over Wu-Palmer concept
//     similarity, blended with BM25 over the expanded query
//     (similarity.go, bm25.go, score.go).
//
// Everything here is written against the Inventory interface, and a crawl
// deliberately uses two implementations at once:
//
//   - the QUERY is disambiguated and expanded against BabelNet, whose
//     vocabulary covers the named entities and jargon a user actually types,
//     and which is affordable because a query is short and its senses are
//     cached across every run of every crawl;
//   - each PAGE is disambiguated against the local WordNet
//     (WordNetInventory), because there are hundreds per run and no metered
//     inventory survives that.
//
// The two are comparable because a BabelNet sense derived from WordNet
// carries its original offset, and Taxonomy measures both on that one
// hierarchy. See WordNetInventory for the full argument.
package wsd

import (
	"context"
	"errors"
)

// ErrInventoryExhausted means the sense inventory has no capacity left right
// now, and that waiting is the remedy rather than retrying.
//
// Declared on the contract rather than by any one implementation so that
// callers can recognise it without importing a particular inventory — the
// crawler must be able to pause on it while remaining ignorant of whether the
// senses came from BabelNet, a local index, or anything else. Implementations
// wrap this; a purely local inventory never returns it.
var ErrInventoryExhausted = errors.New("wsd: sense inventory exhausted")

// Parts of speech, in WordNet's single-letter notation. These are the only
// values the rest of the package accepts.
const (
	POSNoun      = "n"
	POSVerb      = "v"
	POSAdjective = "a"
	POSAdverb    = "r"
)

// Relation types. An inventory maps its own vocabulary onto these; anything
// it does not recognise becomes RelationOther, which the expansion walks at
// the lowest weight rather than discarding.
const (
	RelationHypernym   = "hypernym"
	RelationHyponym    = "hyponym"
	RelationMeronym    = "meronym"
	RelationHolonym    = "holonym"
	RelationDerivation = "derivation"
	RelationOther      = "other"
	// RelationInstance is an INSTANCE of a concept rather than a kind of it:
	// "Aegean Sea" is an instance of sea, "Portugal" of country.
	//
	// Kept distinct because expansion must not follow it. A topical query
	// expanded through instances fills with proper nouns — measured on the
	// query "sea otter habitat conservation along the river bank", following
	// instances produced 634 senses including Acheronian, Tar Heel State and
	// Illinois River, which describe the world rather than the topic and
	// drown BM25 in terms no relevant page will contain.
	RelationInstance = "instance"
)

// Relation is one labelled edge out of a synset.
type Relation struct {
	Target string `json:"target"`
	Type   string `json:"type"`
}

// Synset is one sense: a set of synonymous lemmas sharing a definition.
//
// The unit of everything downstream. A word is ambiguous, a synset is not —
// that is the entire reason this package works in synsets rather than terms.
type Synset struct {
	// ID is the inventory's own identifier (e.g. "bn:00008364n").
	ID  string `json:"id"`
	POS string `json:"pos"`
	// Lemmas are the words that express this sense.
	Lemmas []string `json:"lemmas,omitempty"`
	// Gloss is the definition, which is what Lesk actually compares.
	Gloss string `json:"gloss,omitempty"`
	// WordNetKey is this sense's WordNet 3.0 synset in the local taxonomy's
	// notation ("n02084071"), or empty when the sense has no WordNet
	// counterpart.
	//
	// BabelNet is built by merging WordNet with Wikipedia and carries the
	// original WordNet offsets, which is what makes a BabelNet sense
	// measurable on the WordNet IS-A hierarchy. A BabelNet-only sense (most
	// named entities) has no key and no Wu-Palmer similarity — see
	// Taxonomy.Similarity.
	WordNetKey string `json:"wordnet_key,omitempty"`
	// Relations are the edges the expansion and extended Lesk walk.
	Relations []Relation `json:"relations,omitempty"`
}

// Inventory is a sense inventory: it answers "what can this word mean" and
// "what is this sense".
//
// Implementations are expected to be heavily cached — extended Lesk asks for
// every candidate sense of every content word plus each of their neighbours,
// which is a lot of questions about a small, unchanging body of facts.
type Inventory interface {
	// Senses returns the candidate senses of a lemma for a part of speech,
	// most frequent first where the inventory knows frequency. An unknown
	// lemma is not an error: it returns no senses.
	Senses(ctx context.Context, lemma, pos string) ([]Synset, error)
	// Synset resolves one sense by id, with its relations.
	Synset(ctx context.Context, id string) (Synset, error)
}

// GlossProvider is an optional optimisation for inventories where relations
// cost extra to fetch.
//
// Extended Lesk reads a neighbour's gloss but never walks on from it, so
// paying for that neighbour's own edges is pure waste — and against
// BabelNet's HTTP API, where edges are a second request per synset, it is
// half the cost of disambiguating a word. An inventory that cannot answer
// more cheaply simply does not implement this.
type GlossProvider interface {
	// SynsetGloss resolves a sense by id, with Relations left empty.
	SynsetGloss(ctx context.Context, id string) (Synset, error)
}

// glossOf fetches a synset for its definition alone, using the cheap path
// when the inventory offers one.
func glossOf(ctx context.Context, inv Inventory, id string) (Synset, error) {
	if gp, ok := inv.(GlossProvider); ok {
		return gp.SynsetGloss(ctx, id)
	}
	return inv.Synset(ctx, id)
}
