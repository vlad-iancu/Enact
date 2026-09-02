package wsd

import (
	"context"

	"github.com/fluhus/gostuff/nlp/wordnet"
)

// WordNetInventory is a sense inventory backed by the local WordNet database.
//
// It exists because the two halves of the relevance function have completely
// different economics. The QUERY is disambiguated once per crawl over a
// handful of words, so it can afford BabelNet and benefits enormously from
// it: BabelNet's vocabulary is what lets a query mention a product, a place
// or a piece of jargon that WordNet has never heard of. Every PAGE is
// disambiguated over dozens of terms, hundreds of times per run — which no
// metered inventory survives, and which this one does for free.
//
// The two meet through WordNet keys. A BabelNet sense that came from WordNet
// carries its original offset, and this inventory identifies senses by that
// same key, so a query concept and a page concept are compared on one
// taxonomy without either side knowing where the other came from. A
// BabelNet-only query sense (typically a named entity) has no key and can
// never match a page concept semantically — which is precisely the gap BM25
// over the expanded query covers.
type WordNetInventory struct {
	tax *Taxonomy
}

// NewWordNetInventory returns an inventory over an already-parsed taxonomy.
// It shares the database rather than parsing a second copy: WordNet is over a
// hundred megabytes of heap and entirely read-only.
func NewWordNetInventory(tax *Taxonomy) *WordNetInventory {
	return &WordNetInventory{tax: tax}
}

var (
	_ Inventory     = (*WordNetInventory)(nil)
	_ GlossProvider = (*WordNetInventory)(nil)
)

// Senses returns the senses of a lemma, most frequent first.
//
// Frequency order is not a nicety. WordNet's first sense is a strong
// baseline, and it is what Lesk falls back to when no candidate overlaps the
// context — with an arbitrary order, "cat" resolves to CAT scan and the whole
// measure collapses. Only some senses are ranked, so the ranked ones lead and
// the rest follow in file order.
func (w *WordNetInventory) Senses(_ context.Context, lemma, pos string) ([]Synset, error) {
	if w == nil || w.tax == nil || lemma == "" {
		return nil, nil
	}
	ranked := w.tax.wn.SearchRanked(lemma)[pos]
	all := w.tax.wn.Search(lemma)[pos]

	out := make([]Synset, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, ss := range ranked {
		key := synsetKey(ss)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w.convert(ss))
	}
	for _, ss := range all {
		key := synsetKey(ss)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w.convert(ss))
	}
	return out, nil
}

// Synset resolves one sense by its WordNet key ("n02084071").
func (w *WordNetInventory) Synset(_ context.Context, id string) (Synset, error) {
	if w == nil || w.tax == nil {
		return Synset{}, nil
	}
	key := NormalizeWordNetKey(id)
	ss, ok := w.tax.wn.Synset[key]
	if !ok {
		// An id this database does not have is not an error: expansion walks
		// whatever it can reach and simply stops at the edges.
		return Synset{}, nil
	}
	return w.convert(ss), nil
}

// SynsetGloss is identical to Synset here — relations cost nothing to read
// from a local map, so there is no cheaper path to offer. It is implemented
// so that code written against GlossProvider works with either inventory.
func (w *WordNetInventory) SynsetGloss(ctx context.Context, id string) (Synset, error) {
	return w.Synset(ctx, id)
}

// convert maps a WordNet synset onto the platform's shape.
func (w *WordNetInventory) convert(ss *wordnet.Synset) Synset {
	key := synsetKey(ss)
	out := Synset{
		ID:         key,
		POS:        normalizePOS(ss.Pos),
		Lemmas:     ss.Word,
		Gloss:      ss.Gloss,
		WordNetKey: key,
	}
	for _, p := range ss.Pointer {
		if p.Synset == "" {
			continue
		}
		out.Relations = append(out.Relations, Relation{
			Target: p.Synset,
			Type:   pointerRelation(p.Symbol),
		})
	}
	return out
}

// synsetKey is the database's own key for a synset: part of speech followed
// by offset.
func synsetKey(ss *wordnet.Synset) string {
	if ss == nil || ss.Offset == "" {
		return ""
	}
	return normalizePOS(ss.Pos) + ss.Offset
}

// normalizePOS folds WordNet's adjective satellites into adjectives, which is
// where their data lives.
func normalizePOS(pos string) string {
	if pos == "s" {
		return POSAdjective
	}
	return pos
}

// pointerRelation maps WordNet's pointer symbols onto the platform's relation
// vocabulary.
//
// The symbols are terse and easy to invert: '#' prefixes HOLONYM (has-part)
// and '%' prefixes MERONYM (part-of), which is the opposite of what the
// mnemonics suggest. See wninput(5WN).
func pointerRelation(symbol string) string {
	switch symbol {
	case "@": // hypernym
		return RelationHypernym
	case "~": // hyponym
		return RelationHyponym
	case "@i", "~i": // instance hypernym / hyponym — named entities
		return RelationInstance
	case "%m", "%s", "%p": // member/substance/part meronym
		return RelationMeronym
	case "#m", "#s", "#p": // member/substance/part holonym
		return RelationHolonym
	case "+": // derivationally related form
		return RelationDerivation
	}
	return RelationOther
}
