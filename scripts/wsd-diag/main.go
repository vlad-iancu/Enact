// Command wsd-diag explains how a crawl query is disambiguated, and whether
// the ant colony or the objective is to blame when the answer is wrong.
//
//	make wsd-diag ARGS='-query "opensearch indices, security, syntax and usage" \
//	                    -seed https://dev.to/t/opensearch'
//
// It reproduces exactly what internal/crawler.PrepareRelevance does — the same
// context bag, the same colony, the same objective — and then adds the two
// things a running service cannot afford: an exhaustive search of every
// possible assignment, and a decomposition of the winning score into the
// sense-to-sense agreements that produced it.
//
// Those two answer the question that matters, which is not "is the sense
// wrong" but "which half is wrong". A colony that finds the optimum and still
// returns nonsense is a scoring problem, and no amount of tuning ants and
// evaporation rates will touch it. Every defect found so far has been that
// way round.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sethvargo/go-envconfig"

	"enact/internal/babelnet"
	"enact/internal/crawler"
	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/source"
	"enact/internal/wsd"
)

func main() {
	var (
		query     = flag.String("query", "", "the crawl query to disambiguate (required)")
		seeds     = flag.String("seed", "", "comma-separated seed URLs; their text becomes context, as in a real run")
		text      = flag.String("text", "", "extra context text, instead of or as well as -seed")
		inventory = flag.String("inventory", "wordnet", "wordnet (local, free) or babelnet (metered, needs BABELNET_API_KEY)")
		expect    = flag.String("expect", "", "comma-separated synset ids you believe are correct, to score against")
		maxAssign = flag.Int("max-assignments", wsd.DefaultMaxAssignments, "cap on the exhaustive search")
		ctxTerms  = flag.Int("seed-terms", crawler.DefaultSeedContextTerms, "how many seed lemmas to keep as context")
		maxSenses = flag.Int("max-senses", 0, "candidate senses per word (0 = the shipped default)")
		score     = flag.String("score", "", "comma-separated page URLs to score against the query, "+
			"showing what each half of the score is made of")
		entityW = flag.Float64("entity-weight", 0, "what a name counts for lexically (0 = the shipped default)")
		alpha   = flag.Float64("alpha", wsd.DefaultAlpha, "weight of the semantic half")
	)
	flag.Parse()
	if strings.TrimSpace(*query) == "" {
		fmt.Fprintln(os.Stderr, "wsd-diag: -query is required")
		flag.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	logger := logging.New()

	taxonomy, err := wsd.NewTaxonomy(wsd.Config{WordNetDir: os.Getenv("WORDNET_DIR")})
	if err != nil {
		fail("load WordNet (set WORDNET_DIR; `make wordnet` fetches it): %v", err)
	}

	inv, err := buildInventory(ctx, *inventory, taxonomy, logger)
	if err != nil {
		fail("%v", err)
	}

	// The context bag, built the same way PrepareRelevance builds it. Kept in
	// step by construction where possible and by this comment where not: if
	// the two ever diverge, this tool starts explaining a system that is not
	// the one running.
	idf := wsd.NewIDF()
	terms, err := taxonomy.Analyze(*query)
	if err != nil {
		fail("analyse the query: %v", err)
	}
	if len(terms) == 0 {
		fail("the query has no content words")
	}
	idf.Observe(terms)
	contextBag := make([]string, 0, len(terms)+*ctxTerms)
	for _, term := range terms {
		contextBag = append(contextBag, term.Lemma)
	}

	body := *text
	if *seeds != "" {
		fetched := fetchSeeds(ctx, logger, *seeds)
		fmt.Printf("seeds fetched: %d\n", len(fetched))
		for u, doc := range fetched {
			fmt.Printf("  %-70.70s %6d chars  %q\n", u, len(doc.Text), doc.Title)
		}
		body = strings.TrimSpace(body + "\n\n" + crawler.SeedContext(fetched))
	}
	if strings.TrimSpace(body) != "" {
		seedTerms, err := taxonomy.Analyze(body)
		if err != nil {
			fail("analyse the seed text: %v", err)
		}
		idf.Observe(seedTerms)
		salient := wsd.SalientLemmas(seedTerms, idf.Score, *ctxTerms)
		contextBag = append(contextBag, salient...)
		fmt.Printf("\nseed context (%d lemmas):\n  %s\n", len(salient), strings.Join(salient, " "))
	}

	opts := wsd.ACOOptions{}
	if *maxSenses > 0 {
		opts.Lesk.MaxSenses = *maxSenses
	}
	started := time.Now()
	diagnosis, err := wsd.Diagnose(ctx, inv, taxonomy, terms, contextBag, opts, *maxAssign)
	if diagnosis == nil {
		fail("diagnose: %v", err)
	}
	if err != nil {
		// A metered inventory running out mid-analysis is expected, and what
		// it did resolve is still worth reading.
		fmt.Fprintf(os.Stderr, "\nwarning: the inventory returned an error; this diagnosis is partial: %v\n", err)
	}

	fmt.Printf("\nquery: %q\ninventory: %s   elapsed: %s\n\n", *query, *inventory, time.Since(started).Round(time.Millisecond))
	var expected []string
	if *expect != "" {
		expected = strings.Split(*expect, ",")
	}
	diagnosis.Report(os.Stdout, expected)

	if *score != "" {
		reportScoring(ctx, logger, taxonomy, inv, *query, body, *score, *alpha, *entityW, *ctxTerms)
	}
}

// reportScoring shows what a page's score is made of, and what the entity
// weight is doing to it.
//
// The disambiguation report above explains which senses were chosen; this
// explains what happens next, and the two failure modes are different. A query
// can be understood perfectly and still crawl badly, because the words the
// dictionary could not resolve are invisible to the semantic half and the
// lexical half is left carrying the run alone.
func reportScoring(ctx context.Context, logger *logging.Logger, tax *wsd.Taxonomy,
	inv wsd.Inventory, query, seedText, list string, alpha, entityWeight float64, ctxTerms int) {

	cfg := crawler.RelevanceConfig{
		Alpha: alpha, SeedText: seedText, SeedContextTerms: ctxTerms, EntityWeight: entityWeight,
	}
	rel, err := crawler.PrepareRelevance(ctx, tax, inv, inv, query, cfg)
	if err != nil {
		fail("prepare scoring: %v", err)
	}

	fmt.Println("\n=== LEXICAL VOCABULARY  (what BM25 matches, and what each word is worth)")
	vocab := rel.QueryTerms()
	type weighted struct {
		lemma  string
		weight float64
	}
	ordered := make([]weighted, 0, len(vocab))
	for lemma, w := range vocab {
		ordered = append(ordered, weighted{lemma, w})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].weight != ordered[j].weight {
			return ordered[i].weight > ordered[j].weight
		}
		return ordered[i].lemma < ordered[j].lemma
	})
	shown := 0
	for _, e := range ordered {
		// The query's own words and the strongest expansion; the long tail of
		// weak hypernyms is counted rather than listed.
		if e.weight < 1 {
			break
		}
		tag := ""
		if e.weight > 1 {
			tag = "   <-- name"
		}
		fmt.Printf("   %-28s %.1f%s\n", e.lemma, e.weight, tag)
		shown++
	}
	fmt.Printf("   ... and %d weaker expansion terms\n", len(ordered)-shown)
	fmt.Printf("   %.0f%% of the query's weight is INVISIBLE to the semantic half "+
		"(no sense in the inventory),\n   so coverage is scaled by %.2f and BM25 carries that much more of the decision.\n",
		100*rel.SemanticBlind(), 1-rel.SemanticBlind())

	// The same pages scored with names weighted like everything else, which is
	// the only way to see whether the weighting is doing anything here.
	plainCfg := cfg
	plainCfg.EntityWeight = 1
	plain, err := crawler.PrepareRelevance(ctx, tax, inv, inv, query, plainCfg)
	if err != nil {
		fail("prepare the comparison: %v", err)
	}

	queryTerms, err := tax.Analyze(query)
	if err != nil {
		fail("analyse the query: %v", err)
	}
	fmt.Println("\n=== PAGES")
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		docs := fetchSeeds(ctx, logger, raw)
		if len(docs) == 0 {
			fmt.Printf("\n   %s\n     could not be fetched\n", raw)
			continue
		}
		for url, doc := range docs {
			pageTerms, err := tax.Analyze(doc.Text)
			if err != nil {
				fail("analyse %s: %v", url, err)
			}
			freq := map[string]int{}
			for _, t := range pageTerms {
				freq[t.Lemma]++
			}
			got, err := rel.ScorePage(ctx, doc.Text)
			if err != nil {
				fail("score %s: %v", url, err)
			}
			was, err := plain.ScorePage(ctx, doc.Text)
			if err != nil {
				fail("score %s: %v", url, err)
			}

			fmt.Printf("\n   %.90s\n     %q  (%d words)\n", url, doc.Title, len(pageTerms))
			seen := map[string]bool{}
			for _, term := range queryTerms {
				if seen[term.Lemma] {
					continue
				}
				seen[term.Lemma] = true
				// Marked from the vocabulary rather than the term, because a
				// name learned from the seed pages is not visible on a
				// freshly-analysed query — which is the entire point of
				// learning it there.
				mark := "  "
				if vocab[term.Lemma] > 1 {
					mark = "**"
				}
				fmt.Printf("     %s %-20s x%-4d weight %.1f\n",
					mark, term.Lemma, freq[term.Lemma], vocab[term.Lemma])
			}
			fmt.Printf("     semantic %.3f   lexical %.3f   coverage %.2f   TOTAL %.3f\n",
				got.Semantic, got.Lexical, got.Coverage, got.Total)
			fmt.Printf("     with names weighted 1.0:      lexical %.3f          TOTAL %.3f  (%+.3f)\n",
				was.Lexical, was.Total, got.Total-was.Total)
		}
	}
}

func buildInventory(ctx context.Context, kind string, tax *wsd.Taxonomy, logger *logging.Logger) (wsd.Inventory, error) {
	switch kind {
	case "wordnet":
		return wsd.NewWordNetInventory(tax), nil
	case "babelnet":
		var cfg struct {
			BabelNet   babelnet.Config
			OpenSearch opensearch.Config
			Disabled   bool `env:"DISABLE_BABELNET, default=false"`
		}
		if err := envconfig.Process(ctx, &cfg); err != nil {
			return nil, fmt.Errorf("read the BabelNet configuration from the environment: %w", err)
		}
		// Refused rather than quietly obeyed. A diagnosis of an inventory the
		// deployment does not use is worse than no diagnosis: it explains
		// senses no crawl will ever choose, and does it convincingly.
		if cfg.Disabled {
			return nil, fmt.Errorf("DISABLE_BABELNET is set, so crawls disambiguate against WordNet; " +
				"diagnosing BabelNet would explain a path this deployment does not take " +
				"(use -inventory wordnet, or unset DISABLE_BABELNET to compare them)")
		}
		// The cache is what makes this affordable to run repeatedly: a query
		// already analysed by a real crawl costs nothing to diagnose.
		osClient, err := opensearch.NewClient(cfg.OpenSearch)
		if err != nil {
			return nil, fmt.Errorf("connect to OpenSearch for the BabelNet cache: %w", err)
		}
		return babelnet.New(cfg.BabelNet, osClient, logger), nil
	default:
		return nil, fmt.Errorf("unknown -inventory %q: want wordnet or babelnet", kind)
	}
}

// fetchSeeds goes through the crawler's own fetcher, so robots.txt, the
// per-host delay and the SSRF guard all apply exactly as they would in a run.
func fetchSeeds(ctx context.Context, logger *logging.Logger, list string) map[string]source.Document {
	var urls []string
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, err := url.Parse(raw); err != nil {
			fail("seed %q: %v", raw, err)
		}
		urls = append(urls, raw)
	}
	var cfg crawler.FetchConfig
	if err := envconfig.Process(ctx, &cfg); err != nil {
		fail("read the crawl fetch configuration: %v", err)
	}
	web := crawler.NewWebSource(crawler.NewFetcher(cfg), crawler.WebConfig{})
	return crawler.New(crawler.NewFetcher(cfg), logger).Prefetch(ctx, web, urls)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wsd-diag: "+format+"\n", args...)
	os.Exit(1)
}
