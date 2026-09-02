package crawler

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"

	"enact/internal/wsd"
)

// DefaultSalientTerms is how many of a page's terms are disambiguated.
//
// The quota lever, and also better method. Sense-tagging every lemma of a
// long page costs a great deal for a result dominated by furniture words, and
// the Mihalcea measure was formulated for short texts — giving it a page's
// most characteristic terms is both cheaper and truer to it.
const DefaultSalientTerms = 40

// QueryAnalysis is everything the crawl learned about its query, and the
// first half of a run's report.
//
// Kept in full because it is the only way to understand a crawl's behaviour.
// A crawl that fetched the wrong pages usually did so because a word was read
// in the wrong sense, and no amount of looking at the results reveals that —
// only the disambiguation does.
type QueryAnalysis struct {
	Query     string              `json:"query"`
	Terms     []wsd.Sense         `json:"terms"`
	Expansion []wsd.WeightedSense `json:"expansion"`
	// Coverage is the share of the expanded query that the semantic measure
	// can judge; the rest is carried by BM25 alone. See wsd.Coverage.
	Coverage float64 `json:"coverage"`
	// RequestsSpent is what analysing this query cost at the sense inventory.
	RequestsSpent int `json:"requests_spent"`
	// Degraded reports that the rich inventory was unavailable — out of daily
	// allowance, or refusing the key — and the query was understood against
	// the local WordNet instead.
	//
	// It belongs in the report rather than the logs because it changes what
	// the run means. A degraded analysis has a smaller vocabulary and no
	// encyclopaedic senses at all, so it will read a technical query more
	// literally and the crawl will follow different links. Without this flag
	// that shows up only as a run that inexplicably found worse pages than
	// the one before it.
	Degraded bool `json:"degraded,omitempty"`
	// SeedContext is the vocabulary taken from the starting pages and used to
	// read the query. Reported because it explains the disambiguation: when a
	// word came out in an unexpected sense, the seed's vocabulary is usually
	// why, and there is no other way to see what the crawl was reading the
	// query against.
	SeedContext []string `json:"seed_context,omitempty"`
	// Names are the query's words treated as names, and therefore weighted
	// above the rest. Reported because it is the difference between a crawl
	// that finds the right product and one that finds the right subject: when
	// a crawl drifts onto a competitor's pages, a name missing from this list
	// is the usual reason.
	Names []string `json:"names,omitempty"`
}

// Relevance scores pages and links against a prepared query.
//
// It holds the two inventories the design splits between: the query was
// disambiguated and expanded against BabelNet, whose richer vocabulary is
// what steers the crawl, while pages are disambiguated against the local
// WordNet because there are hundreds of them and no metered inventory
// survives that. See internal/wsd for why the two are comparable.
type Relevance struct {
	tax  *wsd.Taxonomy
	page wsd.Inventory

	analysis      QueryAnalysis
	queryConcepts []wsd.Concept
	queryTerms    map[string]float64

	idf  *wsd.IDF
	bm25 *wsd.BM25

	// queryNames are the lemmas treated as names; nameMissPenalty is what a
	// page keeps when it contains none of them.
	queryNames      map[string]bool
	nameMissPenalty float64

	// semanticBlind is the share of the query's weight that the semantic half
	// can never judge, because the inventory had no sense for those words at
	// all. It scales coverage down; see ScorePage.
	semanticBlind float64

	alpha         float64
	salientTerms  int
	leskOptions   wsd.LeskOptions
	expandOptions wsd.ExpandOptions
}

// RelevanceConfig tunes scoring.
type RelevanceConfig struct {
	// Alpha is the intended weight of the semantic half; see wsd.Combine.
	Alpha float64
	// SalientTerms is how many of a page's terms are disambiguated.
	SalientTerms int
	Lesk         wsd.LeskOptions
	Expand       wsd.ExpandOptions
	// ACO tunes the colony that disambiguates the query. Pages are not
	// disambiguated this way: the colony is worth its cost on the one text
	// that steers the whole crawl, and not on the hundreds it fetches.
	ACO wsd.ACOOptions
	// SeedText is the content of the pages the crawl was told to start from,
	// used as extra context when reading the query.
	//
	// This is the difference between disambiguating five words against each
	// other and disambiguating them against the corpus they were written
	// about. "index" beside "opensearch" and "syntax" is ambiguous; "index"
	// beside a page that says shard, cluster, mapping, query and JSON is not.
	// The seed is the one page a crawl is guaranteed to have, and the operator
	// chose it precisely because it represents the topic.
	SeedText string
	// SeedContextTerms caps how many of the seed's lemmas are kept.
	SeedContextTerms int
	// EntityWeight is what a name in the query counts for lexically, against
	// 1 for an ordinary word. Zero takes wsd.DefaultEntityWeight.
	EntityWeight float64
	// Recognizer is an optional model that finds names the spelling rules
	// cannot. Nil is the ordinary case and costs nothing.
	Recognizer NameRecognizer
	// NameMissPenalty multiplies the score of a page that mentions none of the
	// query's names. Zero takes DefaultNameMissPenalty; 1 disables it.
	NameMissPenalty float64
}

// DefaultNameMissPenalty is what a page keeps when it never mentions the thing
// the query is about.
//
// Weighting a name above ordinary words turned out to be a nudge rather than a
// filter, and the arithmetic says why: one name at weight 3 beside five
// ordinary words at 1 is 37% of the query's weight, so a long page that
// saturates the other 63% wins without the name at all. Measured on
// "opensearch database documentation, syntax and query language", an article
// about building a document search tool with React and Supabase — the word
// "opensearch" appearing nowhere in its text — scored 0.706 lexically against
// 0.517 for a real OpenSearch article, purely by being 1771 words to the
// other's 719.
//
// A query that names something is asking about that thing. A page that never
// mentions it may be about the same subject, but it is not about the same
// thing, and half is roughly what that is worth: enough to lose to any page
// that does mention it, not so much that a passing reference is required to
// survive at all.
const DefaultNameMissPenalty = 0.5

// NameRecognizer finds the names in a piece of text, lowercased.
//
// An interface rather than a dependency so that neither this package nor
// internal/wsd imports ONNX Runtime, CGO, or a hundred megabytes of model.
// internal/ner implements it; a deployment that cannot or does not want to
// satisfy that runs with a nil one and behaves exactly as before.
type NameRecognizer interface {
	Names(text string) map[string]bool
}

// DefaultSeedContextTerms is how much of the seed pages is used as context.
//
// Enough to characterise a topic, small enough that the colony's per-candidate
// context comparison stays cheap and that one long page cannot bury the query
// itself under its own vocabulary.
const DefaultSeedContextTerms = 120

// PrepareRelevance analyses a query once, at the start of a run.
//
// queryInventory is the rich one (BabelNet); pageInventory is the local one.
// They are separate parameters rather than one because the whole economic
// argument of the feature rests on the query being analysed once and pages
// being analysed constantly.
func PrepareRelevance(
	ctx context.Context,
	tax *wsd.Taxonomy,
	queryInventory, pageInventory wsd.Inventory,
	query string,
	cfg RelevanceConfig,
) (*Relevance, error) {
	if cfg.SalientTerms <= 0 {
		cfg.SalientTerms = DefaultSalientTerms
	}
	if cfg.SeedContextTerms <= 0 {
		cfg.SeedContextTerms = DefaultSeedContextTerms
	}
	if cfg.Alpha <= 0 || cfg.Alpha > 1 {
		cfg.Alpha = wsd.DefaultAlpha
	}
	r := &Relevance{
		tax: tax, page: pageInventory,
		idf: wsd.NewIDF(), alpha: cfg.Alpha,
		salientTerms: cfg.SalientTerms,
		leskOptions:  cfg.Lesk, expandOptions: cfg.Expand,
		analysis: QueryAnalysis{Query: query},
	}
	r.bm25 = wsd.NewBM25(r.idf)

	terms, err := tax.Analyze(query)
	if err != nil {
		return nil, err
	}
	// The query is its own first document for idf purposes, so a word the
	// query leans on is not immediately treated as rare noise.
	r.idf.Observe(terms)

	// The seed pages join the context the query is read against. They are
	// observed as corpus documents first so their own idf is meaningful, and
	// only their most characteristic lemmas are kept.
	contextBag := make([]string, 0, len(terms)+cfg.SeedContextTerms)
	for _, term := range terms {
		contextBag = append(contextBag, term.Lemma)
	}
	if strings.TrimSpace(cfg.SeedText) != "" {
		seedTerms, err := tax.Analyze(cfg.SeedText)
		if err != nil {
			return nil, err
		}
		r.idf.Observe(seedTerms)
		seedContext := wsd.SalientLemmas(seedTerms, r.idf.Score, cfg.SeedContextTerms)
		r.analysis.SeedContext = seedContext
		contextBag = append(contextBag, seedContext...)

		// The seed pages tell the query which of its words are names.
		//
		// Nothing about "opensearch database documentation" says the first
		// word names a product — it is lowercase, it is not in any dictionary
		// the crawler consults, and no extractor will touch it. The page the
		// operator pointed at writes it "OpenSearch", where both the extractor
		// and the capitalisation say so plainly. Reading the query's names off
		// the corpus the query is about is better evidence than any rule
		// applied to the query alone, and it costs nothing: those pages were
		// fetched and analysed regardless.
		// Where the names come from, and why it is the model when there is one.
		//
		// Capitalisation is a good signal in a sentence and a terrible one over
		// a whole page. Headings are written in Title Case, so every content
		// word in one tags as a proper noun; code blocks are full of camel case
		// and punctuation. Measured on a real article, the spelling rules
		// harvested `database`, `happy`, `let`, `step` and
		// `deno.env.get("supabase_url` as names — and `database` at triple
		// weight is precisely how an article about vector databases and
		// Supabase, which never mentions OpenSearch in its text at all, came to
		// be stored by a crawl looking for OpenSearch.
		//
		// The model does not make that mistake: seven names on that page
		// against sixty-eight, and none of them a common noun in a heading.
		// So when a model is configured it IS the harvest, rather than an
		// addition to it. Without one the rules are all there is, and their
		// noise is the price of not requiring the model.
		var names map[string]bool
		if cfg.Recognizer != nil {
			names = cfg.Recognizer.Names(cfg.SeedText)
			// The query too, in case it was written with capitals — cheap, and
			// the only way a name absent from the seed pages is ever found.
			for name := range cfg.Recognizer.Names(query) {
				names[name] = true
			}
		} else {
			names = wsd.CollectNames(seedTerms)
		}
		wsd.MarkNames(terms, names)
		r.analysis.Names = namesIn(terms)
	}

	senses, expansion, err := analyzeQuery(ctx, queryInventory, tax, terms, contextBag, cfg)
	// The rich inventory ran dry. Rather than abandon the run, understand the
	// query against the local WordNet — which is offline, unmetered, and
	// already loaded to disambiguate pages.
	//
	// This is a real demotion and not a free one: WordNet has no
	// encyclopaedic senses, so a query naming a product or a technology loses
	// it entirely. It is still far better than the alternative. A paused run
	// contributes nothing at all, and the allowance it is waiting for may not
	// return for most of a day.
	//
	// The whole analysis is redone rather than continued from where it
	// stopped. Splicing WordNet senses onto the BabelNet ones already
	// resolved would leave the expansion holding synset IDs that the surviving
	// inventory cannot walk, and a report mixing the two explains nothing.
	// What the failed attempt did resolve is cached, so the next run gets it
	// back without paying again.
	if errors.Is(err, wsd.ErrInventoryExhausted) && pageInventory != nil && pageInventory != queryInventory {
		if s, e, fallbackErr := analyzeQuery(ctx, pageInventory, tax, terms, contextBag, cfg); fallbackErr == nil {
			senses, expansion, err = s, e, nil
			r.analysis.Degraded = true
		}
		// A fallback that itself failed is not reported: the caller is told
		// about the original exhaustion, which is the actionable one.
	}

	// The senses resolved before an error are kept either way, so a caller
	// that stops here can still report what was understood.
	r.analysis.Terms = senses
	r.analysis.Expansion = expansion
	if err != nil {
		return r, err
	}
	r.queryConcepts = wsd.ConceptsFromExpansion(expansion)
	r.queryTerms = wsd.QueryTerms(terms, expansion, cfg.EntityWeight)
	r.semanticBlind = blindShare(senses, cfg.EntityWeight)
	r.queryNames = map[string]bool{}
	for _, name := range r.analysis.Names {
		r.queryNames[name] = true
	}
	r.nameMissPenalty = cfg.NameMissPenalty
	if r.nameMissPenalty <= 0 {
		r.nameMissPenalty = DefaultNameMissPenalty
	}
	return r, nil
}

// analyzeQuery disambiguates a query and expands it against one inventory.
//
// Split out so the same two steps can be retried wholesale against a second
// inventory; partial results are returned alongside the error because they
// are what the report shows when nothing better is available.
func analyzeQuery(ctx context.Context, inv wsd.Inventory, tax *wsd.Taxonomy,
	terms []wsd.Term, contextBag []string, cfg RelevanceConfig) ([]wsd.Sense, []wsd.WeightedSense, error) {

	// The colony rather than greedy Lesk, because the query's senses have to
	// agree with each other; see wsd.DisambiguateACO. Pages keep the greedy
	// version — there are hundreds of them, and a page's own words are already
	// context enough.
	aco := cfg.ACO
	aco.Lesk = cfg.Lesk
	senses, err := wsd.DisambiguateACO(ctx, inv, tax, terms, contextBag, aco)
	if err != nil {
		return senses, nil, err
	}
	expansion, err := wsd.Expand(ctx, inv, senses, cfg.Expand)
	return senses, expansion, err
}

// Analysis is the query report, including whatever was resolved before an
// error stopped it.
func (r *Relevance) Analysis() QueryAnalysis { return r.analysis }

// SetRequestsSpent records what the query analysis cost, for the report.
func (r *Relevance) SetRequestsSpent(n int) { r.analysis.RequestsSpent = n }

// ScorePage rates a document against the query.
//
// Both halves are computed on the same terms: the page is tagged once, its
// most salient terms are disambiguated for the semantic half, and its full
// term list feeds BM25. Returning the parts as well as the total is what lets
// a report explain a score — 0.4 from BM25 alone means something different
// from 0.4 from semantics alone.
func (r *Relevance) ScorePage(ctx context.Context, text string) (wsd.Score, error) {
	if strings.TrimSpace(text) == "" {
		return wsd.Score{}, nil
	}
	terms, err := r.tax.Analyze(text)
	if err != nil {
		return wsd.Score{}, err
	}
	if len(terms) == 0 {
		return wsd.Score{}, nil
	}
	// The corpus grows as the crawl proceeds, so idf is an estimate from what
	// has been seen. Observing BEFORE scoring means a page contributes to the
	// statistics it is judged by — which is a little circular, but the
	// alternative is scoring the first page against an empty corpus where
	// every term looks maximally rare.
	r.idf.Observe(terms)
	r.bm25.Observe(terms)

	lexical := r.bm25.Score(terms, r.queryTerms)

	salient := wsd.Salient(terms, r.idf.Score, r.salientTerms)
	pageSenses, err := wsd.Disambiguate(ctx, r.page, r.tax, salient, r.leskOptions)
	if err != nil {
		return wsd.Score{}, err
	}
	pageConcepts := wsd.ConceptsFromSenses(pageSenses, r.idf.Score)

	// A page that mentions none of the query's names is not about the thing
	// the query names, however well it matches everything else.
	miss := 1.0
	if r.nameMissPenalty < 1 && len(r.queryNames) > 0 && !mentionsAny(terms, r.queryNames) {
		miss = r.nameMissPenalty
	}

	semantic := wsd.Similarity(r.tax, r.queryConcepts, pageConcepts)
	// Coverage measures the share of the query's CONCEPTS the semantic
	// measure can judge — but a word the inventory had no sense for produces
	// no concept at all, so it is absent from that fraction entirely and
	// coverage comes back 1.00 while the semantic half is blind to the very
	// word the query is about. Measured on "opensearch database
	// documentation, syntax and query language": coverage 1.00, and the
	// highest semantic score of any page went to a PostgreSQL article,
	// because everything the semantic half COULD see fitted it perfectly.
	//
	// Scaling by what the query weighs in total, rather than by what happened
	// to survive into concepts, is what makes coverage mean "how much of this
	// query can meaning-comparison actually judge" again — and through
	// wsd.Combine, that is what hands the decision to BM25 when the answer is
	// no.
	coverage := wsd.Coverage(r.queryConcepts, pageConcepts) * (1 - r.semanticBlind)
	score := wsd.Combine(semantic, lexical, r.alpha, coverage)
	score.Total *= miss
	r.analysis.Coverage = coverage
	return score, nil
}

// ScoreLink estimates how promising an unvisited link is.
//
// Everything available before fetching: the score of the page that linked to
// it, the anchor text, and the words in the URL path. The parent's score
// dominates because it is the only measured quantity — a relevant page's
// links are far likelier to be relevant than a stranger's — while the anchor
// and path adjust it, which is what lets a crawl take "diet and habitat" over
// "terms of service" from the same page.
//
// This is lexical only, deliberately. Disambiguating every anchor on every
// page would multiply the sense lookups by the branching factor for a guess
// that is about to be replaced by a real measurement the moment the page is
// fetched.
func (r *Relevance) ScoreLink(parentScore float64, link Link) float64 {
	hint := r.textOverlap(link.Anchor)
	if pathHint := r.textOverlap(pathWords(link.URL)); pathHint > hint {
		hint = pathHint
	}
	// A weighted blend rather than a sum, so the result stays in [0,1] and
	// remains comparable with the threshold the crawl stops at.
	const parentWeight = 0.6
	return sane(parentWeight*sane(parentScore) + (1-parentWeight)*sane(hint))
}

// sane maps a non-finite or out-of-range score onto something usable.
//
// A belt-and-braces guard at the boundary, not a substitute for computing the
// value correctly. A NaN priority is uniquely destructive here: it compares
// false against every other value, so container/heap silently stops being
// ordered, and encoding/json refuses to marshal it, so the entire run report
// fails to save — one bad anchor on one page loses the whole crawl. That is
// too much leverage to leave to the arithmetic being right everywhere.
func sane(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// textOverlap is the share of the expanded query's weight present in a short
// piece of text.
//
// Deliberately crude — no tagging, no disambiguation — because it runs on
// every anchor of every page. It is a hint for ordering, not a measurement.
func (r *Relevance) textOverlap(text string) float64 {
	if text == "" || len(r.queryTerms) == 0 {
		return 0
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(c rune) bool {
		return !('a' <= c && c <= 'z') && !('0' <= c && c <= '9')
	})
	if len(words) == 0 {
		return 0
	}
	var matched float64
	seen := map[string]bool{}
	for _, w := range words {
		if len(w) < 3 || seen[w] {
			continue
		}
		seen[w] = true
		// Lemmatising each word would be more accurate; matching the surface
		// against the expanded query's lemmas is close enough for a hint and
		// costs nothing.
		if weight, ok := r.queryTerms[w]; ok {
			matched += weight
		} else if weight, ok := r.queryTerms[r.tax.Lemmatize(w, wsd.POSNoun)]; ok {
			matched += weight
		}
	}
	// Every word was too short or a repeat, so there is nothing to normalise
	// by. Without this guard the division below is 0/0, which is NaN — and a
	// NaN priority is not merely wrong, it is corrosive: it compares false
	// against everything so it corrupts the frontier's ordering, and
	// encoding/json refuses to marshal it, so the whole run report becomes
	// unsaveable. Anchors like "»" or "3D" are common enough that this
	// happened on the first real crawl.
	if len(seen) == 0 {
		return 0
	}
	// Saturating on the matched weight, NOT divided by the anchor's length.
	//
	// Dividing was the obvious thing and it was backwards: it asks "what
	// FRACTION of this anchor is query vocabulary", which punishes exactly the
	// anchors worth following. Measured on a real crawl, "Your AI Agent Just
	// Read the man Pages. OpenSearch Agent Skills" scored one match over ten
	// words = 0.1, while "Terms of use" scored one over three = 0.33 — so the
	// crawler preferred the terms-of-service page to the article.
	//
	// The question that actually matters is "does this anchor contain query
	// vocabulary, and how much", which is the matched weight itself. It is
	// unbounded, so it saturates rather than clamping: one full-weight match
	// gives 0.5, two give 0.67, and a long anchor is never penalised for
	// being descriptive.
	return matched / (matched + 1)
}

// pathWords turns a URL path into readable words: "/wiki/Sea_otter-diet" ->
// "wiki Sea otter diet".
//
// URL paths are frequently the best description of a page available before
// fetching it, and on many sites the only one — a bare "read more" anchor
// pointing at /species/enhydra-lutris/habitat says plenty.
func pathWords(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path := u.Path
	// Drop a file extension, which is never a topical word.
	if i := strings.LastIndex(path, "."); i > strings.LastIndex(path, "/") {
		path = path[:i]
	}
	return strings.Join(strings.FieldsFunc(path, func(c rune) bool {
		return c == '/' || c == '-' || c == '_' || c == '+' || c == '.'
	}), " ")
}

// blindShare is the fraction of the query's lexical weight carried by words
// the sense inventory could not resolve at all.
//
// Those words are exactly the ones the semantic half cannot represent, and —
// because they are names — usually the ones the query is most about. Weighting
// them the way BM25 does keeps one notion of "how important is this word"
// across both halves of the score.
func blindShare(senses []wsd.Sense, entityWeight float64) float64 {
	if entityWeight <= 0 {
		entityWeight = wsd.DefaultEntityWeight
	}
	var total, blind float64
	seen := map[string]bool{}
	for _, sense := range senses {
		key := sense.Term.POS + ":" + sense.Term.Lemma
		if seen[key] {
			continue
		}
		seen[key] = true
		weight := 1.0
		if sense.Term.Entity {
			weight = entityWeight
		}
		total += weight
		if sense.SynsetID == "" {
			blind += weight
		}
	}
	if total == 0 {
		return 0
	}
	return blind / total
}

// QueryTerms is the lexical vocabulary a page is matched against, with the
// weight each word carries. Names weigh more than ordinary words; see
// wsd.QueryTerms.
//
// A copy, because it is exposed for reporting and a caller that mutated it
// would silently change how every subsequent page scores.
func (r *Relevance) QueryTerms() map[string]float64 {
	out := make(map[string]float64, len(r.queryTerms))
	for lemma, weight := range r.queryTerms {
		out[lemma] = weight
	}
	return out
}

// SemanticBlind is the share of the query's weight that the semantic half
// cannot judge at all, because the inventory had no sense for those words.
//
// The single most useful number for explaining a crawl that went somewhere
// unexpected. At 0.6 — a query whose name the dictionary has never heard of —
// meaning-comparison is deciding on the leftovers, and the lexical half is
// carrying the run whether or not alpha says so.
func (r *Relevance) SemanticBlind() float64 { return r.semanticBlind }

// namesIn lists the distinct query terms treated as names, in order.
func namesIn(terms []wsd.Term) []string {
	seen := map[string]bool{}
	var out []string
	for _, term := range terms {
		if term.Entity && !seen[term.Lemma] {
			seen[term.Lemma] = true
			out = append(out, term.Lemma)
		}
	}
	return out
}

// mentionsAny reports whether a page uses any of the query's names.
//
// Lemmas, so "indices" finds "index" and a plural does not hide a match; and
// any ONE name is enough, because a query naming several things is usually
// asking about their intersection but a page about one of them is still on
// topic.
func mentionsAny(terms []wsd.Term, names map[string]bool) bool {
	for _, term := range terms {
		if names[term.Lemma] {
			return true
		}
	}
	return false
}
