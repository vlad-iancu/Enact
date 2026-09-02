package wsd

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/jdkato/prose/v2"
)

// Term is one content word of a text, tagged and reduced to its lemma.
type Term struct {
	Surface string `json:"surface"`
	Lemma   string `json:"lemma"`
	POS     string `json:"pos"`
	// Entity marks a name: a product, a technology, an acronym, a person —
	// a word that identifies one thing rather than describing a category.
	//
	// It exists because such words are worth more than the rest of a query
	// and the scoring had no way to know it. "opensearch database
	// documentation, syntax and query language" has one word that decides
	// whether a page is relevant and five that any database page satisfies;
	// weighting all six equally is how a Postgres page comes back.
	//
	// See QueryTerms for what the mark is worth.
	Entity bool `json:"entity,omitempty"`
}

// posFromPenn maps the Penn Treebank tags prose emits onto WordNet's
// single-letter parts of speech. Anything absent is not a content word and is
// dropped: determiners, prepositions, pronouns, conjunctions and punctuation
// carry no sense to disambiguate.
//
// Adverbs are deliberately excluded even though WordNet has them — the spec
// keeps nouns, verbs and adjectives, and adverbs contribute little to topical
// relevance while costing an inventory lookup each.
var posFromPenn = map[string]string{
	"NN": POSNoun, "NNS": POSNoun, "NNP": POSNoun, "NNPS": POSNoun,
	"VB": POSVerb, "VBD": POSVerb, "VBG": POSVerb, "VBN": POSVerb,
	"VBP": POSVerb, "VBZ": POSVerb,
	"JJ": POSAdjective, "JJR": POSAdjective, "JJS": POSAdjective,
}

// Analyze tokenizes, POS-tags and lemmatises a text, returning its content
// words in order.
//
// Duplicates are kept: a word used five times is five terms, because term
// frequency is what BM25 and the salience ranking are computed from.
func (t *Taxonomy) Analyze(text string) ([]Term, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	// Entity extraction roughly doubles the cost of tagging — measured at 59ms
	// against 129ms for a 5kB page — and it is paid on every page of every
	// crawl. It earns that back in one specific way: it is the only thing here
	// that sees a name as a UNIT. "Amazon OpenSearch Service" is otherwise
	// three unrelated words, and a page about Amazon Web Services matches it
	// exactly as well as a page about the thing being asked for.
	//
	// What it does NOT do is worth being clear about, because it is easy to
	// expect otherwise: prose's model is driven by capitalisation. On a
	// lowercase query — "opensearch database documentation", which is how
	// people actually type them — it finds nothing at all, and the
	// dictionary-absence rule below is still what catches the name.
	doc, err := prose.NewDocument(text, prose.WithSegmentation(false))
	if err != nil {
		return nil, fmt.Errorf("wsd: tag text: %w", err)
	}
	tokens := doc.Tokens()
	named, spans := entitySpans(doc)
	terms := make([]Term, 0, len(tokens))
	for _, tok := range tokens {
		surface := strings.TrimSpace(tok.Text)
		if !isWord(surface) {
			continue
		}
		pos, ok := posFromPenn[tok.Tag]
		if !ok {
			// The tag says this is not a content word. Believe it, unless the
			// dictionary has never heard of the word — because then the tag is
			// a guess with nothing behind it, and the guesses are wrong in the
			// most damaging possible way.
			//
			// Measured: in "opensearch shard allocation tuning" the tagger
			// called `opensearch` an ADVERB, adverbs are excluded, and the one
			// word the query was about vanished before scoring ever saw it —
			// no lexical match, no sense, nothing. The same word in
			// "opensearch database documentation" tags as a noun and works.
			//
			// An out-of-vocabulary token is overwhelmingly a name, an acronym
			// or jargon, so a noun is both the safest default and the usual
			// one in a tagging pipeline.
			if glossStop[strings.ToLower(surface)] || !t.unknownWord(t.Lemmatize(surface, POSNoun)) {
				continue
			}
			pos = POSNoun
		}
		lemma := t.Lemmatize(surface, pos)
		if lemma == "" {
			continue
		}
		terms = append(terms, Term{
			Surface: surface, Lemma: lemma, POS: pos,
			// The extractor's verdict is accepted only away from the first
			// token, for the same reason the capitalisation rule is: at the
			// start of a text a capital letter is evidence of nothing, and
			// prose duly labels the leading word of "Database documentation
			// and query syntax" a name. A multi-word name that begins the text
			// still survives as its own span term below.
			// The extractor's verdict is accepted only away from the first
			// token, for the same reason the capitalisation rule is: at the
			// start of a text a capital letter is evidence of nothing, and
			// prose duly labels the leading word of "Database documentation
			// and query syntax" a name. A multi-word name that begins the text
			// still survives as its own span term below.
			Entity: t.isEntity(surface, tok.Tag, len(terms) == 0,
				len(terms) > 0 && named[strings.ToLower(surface)]),
		})
	}
	// Multi-word names are appended as single terms as well as their parts, so
	// BM25 can match "amazon opensearch service" as the one thing it names
	// while the words still count individually. Both sides of a comparison run
	// through here, so a query span and a page span meet as the same lemma.
	for _, span := range spans {
		terms = append(terms, Term{Surface: span, Lemma: span, POS: POSNoun, Entity: true})
	}
	return terms, nil
}

// entitySpans returns the words prose considered part of a name, and the
// multi-word names themselves.
//
// Single-word entities are not returned as spans: they are already a term, and
// adding them again would double their frequency in every count.
func entitySpans(doc *prose.Document) (map[string]bool, []string) {
	words := map[string]bool{}
	var spans []string
	for _, entity := range doc.Entities() {
		parts := strings.Fields(entity.Text)
		if len(parts) == 0 {
			continue
		}
		for _, part := range parts {
			words[strings.ToLower(strings.Trim(part, ".,;:!?()[]\"'"))] = true
		}
		if span, ok := usableSpan(parts); ok {
			spans = append(spans, span)
		}
	}
	return words, spans
}

// MaxSpanWords bounds a multi-word name. Real ones are short — "Amazon
// OpenSearch Service", "Google Cloud Platform" — and anything longer is the
// extractor having lost its place in badly extracted page text.
const MaxSpanWords = 4

// usableSpan decides whether an extracted span is a name worth keeping as a
// term of its own.
//
// Necessary because entity extraction was measured on a real crawled page and
// its spans included "data is damning", "hands fire jump" and a
// seventeen-word run of site navigation beginning "dev organization accounts
// dev showcase about contact". Page text is chrome, boilerplate and broken
// sentences, and a model trained on newswire drifts badly across it. Each of
// those spans was becoming a term: counted in the page's length, matchable by
// BM25, eligible for the salience budget.
//
// Two cheap guards catch essentially all of it — a length bound, and the
// requirement that a name contains no function words, which no real one does.
func usableSpan(parts []string) (string, bool) {
	if len(parts) < 2 || len(parts) > MaxSpanWords {
		return "", false
	}
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		word := strings.ToLower(strings.Trim(part, ".,;:!?()[]{}\"'"))
		if word == "" || glossStop[word] || !isWord(word) {
			return "", false
		}
		clean = append(clean, word)
	}
	return strings.Join(clean, " "), true
}

// isWord rejects tokens that are punctuation, numbers or single letters.
// A single letter is never a useful sense, and a number has no sense at all.
func isWord(s string) bool {
	if len([]rune(s)) < 2 {
		return false
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 2
}

// Salient returns the n most informative distinct terms of a text, most
// informative first, along with their frequencies.
//
// This is the quota lever. Disambiguating every distinct lemma of a long page
// would cost hundreds of inventory lookups for a result dominated by
// furniture words; the Mihalcea measure was formulated for short texts, and
// giving it a page's most characteristic terms is both cheaper and truer to
// the method.
//
// "Informative" is tf-idf where idf comes from the corpus seen so far, so the
// first page of a crawl ranks by frequency alone and later pages increasingly
// favour what makes them different from their neighbours.
func Salient(terms []Term, idf func(lemma string) float64, n int) []Term {
	if n <= 0 || len(terms) == 0 {
		return nil
	}
	type entry struct {
		term  Term
		count int
	}
	byKey := make(map[string]*entry, len(terms))
	order := make([]string, 0, len(terms))
	for _, term := range terms {
		key := term.POS + ":" + term.Lemma
		if e, ok := byKey[key]; ok {
			e.count++
			continue
		}
		byKey[key] = &entry{term: term, count: 1}
		order = append(order, key)
	}
	scored := make([]struct {
		term  Term
		score float64
	}, 0, len(order))
	for _, key := range order {
		e := byKey[key]
		weight := 1.0
		if idf != nil {
			weight = idf(e.term.Lemma)
		}
		scored = append(scored, struct {
			term  Term
			score float64
		}{e.term, float64(e.count) * weight})
	}
	// Partial selection sort: n is small (tens) against a few hundred
	// candidates, so this beats sorting the whole slice and is stable in the
	// order terms first appeared.
	if n > len(scored) {
		n = len(scored)
	}
	out := make([]Term, 0, n)
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[best].score {
				best = j
			}
		}
		scored[i], scored[best] = scored[best], scored[i]
		out = append(out, scored[i].term)
	}
	return out
}

// isEntity decides whether a term names something, from the evidence in the
// text itself.
//
// The dictionary is deliberately NOT consulted. Treating "absent from WordNet"
// as "is a name" was the previous rule and it was a bad proxy: WordNet is a
// general-English lexicon from 2006, so it also lacks `rebalancing`, every
// typo a user makes, and most words coined this century. Measured, it marked
// `documetation` and `databse` as product names and gave them triple weight —
// and because that weight enters BM25's normalising ceiling, a typo dragged
// down the score of every page in the crawl.
//
// What is left is evidence rather than absence:
//
//   - the extractor found a name here (see entitySpans);
//   - the tagger called it a proper noun AND it is capitalised, away from the
//     first token where a capital means only that a sentence began;
//   - it is spelled like a name — see looksLikeName.
//
// None of these fire on a lowercase mention of a name in a query. That gap is
// closed from the other direction, by harvesting names from the pages a crawl
// was pointed at: see CollectNames.
func (t *Taxonomy) isEntity(surface, tag string, first, extracted bool) bool {
	if extracted {
		return true
	}
	if looksLikeName(surface) {
		return true
	}
	if first || (tag != "NNP" && tag != "NNPS") {
		return false
	}
	r := []rune(surface)
	return len(r) > 0 && unicode.IsUpper(r[0])
}

// looksLikeName recognises the shapes technical names take, independently of
// any dictionary.
//
// Three signals, each of which ordinary English words essentially never show:
// a capital letter inside the word (OpenSearch, gRPC, PostgreSQL), letters
// mixed with digits (IPv6, S3, cohere-768-1m), and an all-capital run (ORM,
// CDC, API). They are spelling conventions the industry uses precisely BECAUSE
// they mark something as a name, which is what makes them safe to read that
// way.
func looksLikeName(surface string) bool {
	var upper, lower, digit int
	for i, r := range surface {
		switch {
		case unicode.IsDigit(r):
			digit++
		case unicode.IsUpper(r):
			upper++
			if i > 0 {
				// A capital after the first letter: camel case, or an acronym
				// glued to a word.
				return true
			}
		case unicode.IsLower(r):
			lower++
		}
	}
	if digit > 0 && upper+lower > 0 {
		return true
	}
	return upper >= 2 && lower == 0
}

// CollectNames gathers the names a body of text asserts, for use on text that
// does not assert them itself.
//
// This is what replaces the dictionary-absence rule, and it is better evidence
// by some distance. A person types "opensearch database documentation" in
// lowercase, where nothing about the spelling says the first word is a name —
// but the page they pointed the crawl at writes it "OpenSearch", where both
// the extractor and the capitalisation say so plainly. The query's names are
// therefore read off the corpus the query is about, rather than guessed from
// what a dictionary happens to be missing.
//
// A name has to be capitalised CONSISTENTLY to count, which is the whole of
// the difference between a name and a headline. Titles are written in Title
// Case, so every content word in one is capitalised and every one of them tags
// as a proper noun: measured, the heading "Opensearch as a Vector Database for
// Semantic Search" yielded `database`, `vector`, `semantic` and `search` as
// names, `database` was weighted triple, and an article about vector databases
// and Supabase — no mention of OpenSearch anywhere in its text — was stored by
// a crawl looking for OpenSearch.
//
// A document that also writes the word in lower case has told us the capital
// was a convention of the heading. A document that only ever writes
// "OpenSearch" has told us the opposite.
//
// Multi-word names are excluded: they are matched as spans in their own right,
// and their parts are already marked individually.
func CollectNames(terms []Term) map[string]bool {
	type tally struct{ upper, lower int }
	counts := make(map[string]*tally)
	marked := make(map[string]bool)
	for _, term := range terms {
		if strings.Contains(term.Lemma, " ") {
			continue
		}
		count, ok := counts[term.Lemma]
		if !ok {
			count = &tally{}
			counts[term.Lemma] = count
		}
		if r := []rune(term.Surface); len(r) > 0 && unicode.IsUpper(r[0]) {
			count.upper++
		} else {
			count.lower++
		}
		if term.Entity {
			marked[term.Lemma] = true
		}
	}
	names := make(map[string]bool)
	for lemma := range marked {
		if count := counts[lemma]; count != nil && count.upper > count.lower {
			names[lemma] = true
		}
	}
	return names
}

// MarkNames applies names gathered elsewhere to a text's own terms, matching
// case-insensitively — which is the whole point, since the case is exactly
// what the query is missing.
func MarkNames(terms []Term, names map[string]bool) {
	for i := range terms {
		if names[terms[i].Lemma] {
			terms[i].Entity = true
		}
	}
}

// unknownWord reports that the dictionary has no entry for a lemma under ANY
// part of speech.
//
// Across all parts of speech rather than the one the tagger guessed, because
// the tagger guesses badly on the input this actually runs against. A crawl
// query is a fragment — "windows server security hardening" — and a tagger
// trained on sentences called `server` a verb there; WordNet has no verb
// `server`, so the word was marked a name and given three times the weight of
// everything around it. In an ordinary sentence the same word tags as a noun
// and behaves. Nothing about "is this a name" should turn on that.
//
// Adverbs are included here even though Analyze discards them as terms: the
// question is whether the DICTIONARY knows the word, not whether this pipeline
// would keep it. Leaving them out reported every real adverb as an unknown
// word, which is how "quickly" came to be a candidate for a product name.
//
// Four map lookups, on a path that already does several.
func (t *Taxonomy) unknownWord(lemma string) bool {
	return t.senses(lemma, POSNoun) == 0 &&
		t.senses(lemma, POSVerb) == 0 &&
		t.senses(lemma, POSAdjective) == 0 &&
		t.senses(lemma, POSAdverb) == 0
}
