// Package babelnet is the BabelNet sense inventory: an HTTP client for
// babelnet.io, a permanent cache in front of it, and a daily request budget.
//
// BabelNet is used for the QUERY side of a crawl, and only that side. Its
// vocabulary is far richer than WordNet's — it merges WordNet with Wikipedia,
// so it knows named entities, domain jargon and multiword expressions — which
// is exactly what a user's query is likely to contain, and what decides the
// direction of the entire crawl. Pages are disambiguated against the local
// WordNet instead (wsd.WordNetInventory), because there are hundreds of them
// per run and no metered inventory survives that.
//
// The split is affordable because the free tier allows 1000 requests per day
// and the algorithms in internal/wsd are hungry: disambiguating one word
// costs a sense lookup, plus two requests per candidate sense, plus one per
// neighbour consulted. A ten-word query is a few hundred requests once; a
// page would be a thousand every time.
//
// Three further things keep even the query side cheap:
//
//   - Cache: every answer is stored forever. BabelNet only changes between
//     releases, so there is nothing to invalidate, and the cache is shared by
//     every crawl in the deployment rather than scoped to an organization —
//     it holds public lexical facts, not anybody's data. A crawl's query
//     rarely changes, so the second run of a crawl costs nothing at all.
//   - Budget: spend is counted per UTC day and refused past the limit with a
//     sentinel error, so a crawl stops cleanly and resumes tomorrow instead
//     of failing halfway through with a network error.
//   - Cheap gloss path: SynsetGloss skips the second request when only a
//     definition is wanted (see wsd.GlossProvider).
package babelnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"enact/internal/requesthelper"
	"enact/internal/wsd"
)

// Config holds the BabelNet connection and the limits placed on it.
type Config struct {
	// APIKey is the babelnet.io key. Empty disables the inventory entirely
	// rather than failing per request, so a deployment that has not signed up
	// gets one clear error at startup.
	APIKey  string        `env:"BABELNET_API_KEY"`
	BaseURL string        `env:"BABELNET_BASE_URL, default=https://babelnet.io/v9"`
	Timeout time.Duration `env:"BABELNET_TIMEOUT, default=20s"`
	// SearchLang is the language of the lemmas looked up.
	SearchLang string `env:"BABELNET_SEARCH_LANG, default=EN"`
	// DailyBudget is how many requests may be spent per UTC day. The free
	// tier is 1000; set it lower to leave room for other consumers of the
	// same key.
	DailyBudget int `env:"BABELNET_DAILY_BUDGET, default=1000"`
	// MaxSenses caps how many candidate senses are RESOLVED per lemma, which
	// is the single biggest lever on request cost: each one costs a getSynset
	// plus a getOutgoingEdges.
	//
	// The cap is applied here rather than only in the disambiguator because
	// by then the requests have already been spent. "otter" has 25 senses;
	// resolving all of them to let Lesk choose between four would cost fifty
	// requests — five per cent of a day, for one word.
	//
	// Ten rather than the original four, matching wsd.DefaultLeskOptions:
	// four made the disambiguator structurally unable to find the technical
	// sense of an ordinary word, which is the whole job. It is genuinely
	// expensive — a twenty-term query costs roughly 400 requests cold, some
	// forty per cent of a free-tier day — but it is paid once per distinct
	// query and cached permanently, so it is a first-run cost, not a per-run
	// one. Lower it if the key is shared with something else.
	MaxSenses int `env:"BABELNET_MAX_SENSES, default=10"`
	// CacheIndex holds the permanent answers.
	CacheIndex string `env:"OPENSEARCH_INDEX_BABELNET_CACHE, default=enact-babelnet-cache"`
}

// ErrBudgetExhausted means the day's request allowance is gone.
//
// It is a sentinel rather than a failure because it is an expected part of
// operating on the free tier: a crawl that hits it has not gone wrong, it has
// run out of road for today. The orchestrator catches it, records the run as
// partial with its frontier intact, and resumes on the next sweep.
//
// It wraps wsd.ErrInventoryExhausted so that a caller can recognise "the
// senses ran out" without importing this package — the crawler pauses on the
// general condition and stays ignorant of which inventory it was using.
var ErrBudgetExhausted = fmt.Errorf("%w: daily request budget exhausted", wsd.ErrInventoryExhausted)

// ErrNoAPIKey means the inventory was used without being configured.
var ErrNoAPIKey = errors.New("babelnet: BABELNET_API_KEY is not set")

// ErrKeyRejected is ErrBudgetExhausted's twin, for the case where the key
// itself looks wrong.
//
// BabelNet answers a single 403 to both "this key is invalid" and "today's
// coins are gone" — its own message says so: "Your key is not valid or the
// daily requests limit has been reached." Both must stop the crawl, so the
// behaviour is identical; the difference is what the operator is told. A run
// that reports "budget exhausted" every day forever, when the real problem is
// a typo in the key, is a failure nobody can diagnose from the report.
//
// It wraps ErrBudgetExhausted so callers that only care about "stop and
// resume later" need not distinguish them.
var ErrKeyRejected = fmt.Errorf(
	"%w: BabelNet refused the key on this deployment's first request of the day, "+
		"which usually means BABELNET_API_KEY is wrong rather than spent", ErrBudgetExhausted)

// errRefused is the client's internal signal for a 403/401; the Inventory
// turns it into one of the two exported errors above.
var errRefused = errors.New("babelnet: request refused")

// client talks to babelnet.io.
type client struct {
	http       *http.Client
	baseURL    string
	apiKey     string
	searchLang string
}

func newClient(cfg Config) *client {
	return &client{
		http: &http.Client{
			Transport: requesthelper.NewTransport(nil),
			Timeout:   cfg.Timeout,
		},
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		searchLang: cfg.SearchLang,
	}
}

// synsetIDResponse is one element of getSynsetIds.
type synsetIDResponse struct {
	ID  string `json:"id"`
	POS string `json:"pos"`
}

// synsetResponse is getSynset. Only the fields this platform reads are
// modelled; BabelNet returns a great deal more (images, categories, domains,
// translations) that would be stored and never looked at.
type synsetResponse struct {
	Senses []struct {
		Properties struct {
			FullLemma string `json:"fullLemma"`
			Language  string `json:"language"`
			POS       string `json:"pos"`
		} `json:"properties"`
	} `json:"senses"`
	Glosses []struct {
		Gloss    string `json:"gloss"`
		Language string `json:"language"`
	} `json:"glosses"`
	// WNOffsets are the WordNet synsets this sense was built from. Present
	// only for senses derived from WordNet, which is exactly the set that can
	// be measured on its taxonomy.
	//
	// BabelNet returns one entry PER WORDNET VERSION — typically both
	// Open English WordNet ("oewn:02483504n", source OEWN) and WordNet 3.0
	// ("wn:02444819n", source WN). Only the 3.0 entry is usable here, because
	// 3.0 is what the local taxonomy is; an OEWN id looks perfectly valid and
	// resolves to nothing, which would silently zero every similarity.
	WNOffsets []wnOffset `json:"wnOffsets"`
}

type wnOffset struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

// wordNet30Key picks the WordNet 3.0 offset out of the versions BabelNet
// offers, and returns it in the local taxonomy's notation.
func wordNet30Key(offsets []wnOffset) string {
	for _, o := range offsets {
		if o.Source == "WN" || o.Version == "WN_30" {
			return wsd.NormalizeWordNetKey(o.ID)
		}
	}
	// No 3.0 mapping: a sense that exists only in a newer WordNet, or only in
	// Wikipedia. It has no place in the local hierarchy, and saying so
	// honestly is better than returning an id that will never resolve.
	return ""
}

// edgeResponse is one element of getOutgoingEdges.
type edgeResponse struct {
	Target  string `json:"target"`
	Pointer struct {
		RelationGroup string `json:"relationGroup"`
		Name          string `json:"name"`
		ShortName     string `json:"shortName"`
	} `json:"pointer"`
}

// get performs one API call and decodes the body into out.
func (c *client) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	if c.apiKey == "" {
		return ErrNoAPIKey
	}
	params.Set("key", c.apiKey)
	target := c.baseURL + "/" + endpoint + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("babelnet: build %s request: %w", endpoint, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("babelnet: %s: %w", endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("babelnet: decode %s response: %w", endpoint, err)
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// One status, two causes — see ErrKeyRejected. Either way the crawl
		// must stop: continuing would hammer a key that is at best spent.
		return errRefused
	default:
		return fmt.Errorf("babelnet: %s: unexpected status %d", endpoint, resp.StatusCode)
	}
}

// sourceWordNet restricts a sense lookup to WordNet-derived synsets.
const sourceWordNet = "WN"

func (c *client) synsetIDs(ctx context.Context, lemma, pos, source string) ([]synsetIDResponse, error) {
	params := url.Values{}
	params.Set("lemma", lemma)
	params.Set("searchLang", c.searchLang)
	if p := babelPOS(pos); p != "" {
		params.Set("pos", p)
	}
	if source != "" {
		params.Set("source", source)
	}
	var out []synsetIDResponse
	if err := c.get(ctx, "getSynsetIds", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) synset(ctx context.Context, id string) (synsetResponse, error) {
	params := url.Values{}
	params.Set("id", id)
	params.Set("targetLang", c.searchLang)
	var out synsetResponse
	err := c.get(ctx, "getSynset", params, &out)
	return out, err
}

func (c *client) edges(ctx context.Context, id string) ([]edgeResponse, error) {
	params := url.Values{}
	params.Set("id", id)
	var out []edgeResponse
	if err := c.get(ctx, "getOutgoingEdges", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// babelPOS maps WordNet's single letters onto BabelNet's spelled-out parts of
// speech. Filtering server-side matters: without it, "run" returns every
// sense of every part of speech and the candidate cap then discards the ones
// actually wanted.
func babelPOS(pos string) string {
	switch pos {
	case wsd.POSNoun:
		return "NOUN"
	case wsd.POSVerb:
		return "VERB"
	case wsd.POSAdjective:
		return "ADJ"
	case wsd.POSAdverb:
		return "ADV"
	}
	return ""
}

// relationType maps BabelNet's relation groups onto the platform's vocabulary.
func relationType(group, name string) string {
	switch strings.ToUpper(group) {
	case "HYPERNYM":
		return wsd.RelationHypernym
	case "HYPONYM":
		return wsd.RelationHyponym
	case "MERONYM":
		return wsd.RelationMeronym
	case "HOLONYM":
		return wsd.RelationHolonym
	}
	// Derivational forms arrive as an ungrouped pointer, identified by name.
	if strings.Contains(strings.ToLower(name), "derivationally") {
		return wsd.RelationDerivation
	}
	return wsd.RelationOther
}

// toSynset converts an API response into the platform's shape.
func toSynset(id string, s synsetResponse, edges []edgeResponse, lang string) wsd.Synset {
	out := wsd.Synset{ID: id, POS: posFromID(id)}
	for _, sense := range s.Senses {
		if sense.Properties.Language != "" && !strings.EqualFold(sense.Properties.Language, lang) {
			continue
		}
		if lemma := cleanLemma(sense.Properties.FullLemma); lemma != "" {
			out.Lemmas = append(out.Lemmas, lemma)
		}
	}
	for _, gloss := range s.Glosses {
		if gloss.Language != "" && !strings.EqualFold(gloss.Language, lang) {
			continue
		}
		if g := strings.TrimSpace(gloss.Gloss); g != "" {
			// The first gloss is the best one BabelNet has; the rest are
			// alternative definitions from other sources and add noise to the
			// overlap more than signal.
			out.Gloss = g
			break
		}
	}
	out.WordNetKey = wordNet30Key(s.WNOffsets)
	for _, edge := range edges {
		if edge.Target == "" {
			continue
		}
		out.Relations = append(out.Relations, wsd.Relation{
			Target: edge.Target,
			Type:   relationType(edge.Pointer.RelationGroup, edge.Pointer.Name),
		})
	}
	out.Relations = orderRelations(out.Relations)
	return out
}

// relationPriority ranks relation types by how much they say about meaning.
//
// Taxonomic edges place a concept; "other" edges only say two concepts
// co-occur somewhere. The distinction is not academic: the BabelNet synset
// for the animal "otter" carries 1739 relations, of which 8 are hypernyms,
// 13 are hyponyms and 1716 are ungrouped relatedness. Taking edges in
// arrival order would spend every hop of the expansion, and every slot of
// the extended gloss, on the 1716.
func relationPriority(t string) int {
	switch t {
	case wsd.RelationHypernym:
		return 0
	case wsd.RelationHyponym:
		return 1
	case wsd.RelationMeronym, wsd.RelationHolonym:
		return 2
	case wsd.RelationDerivation:
		return 3
	case wsd.RelationInstance:
		return 5 // never expanded, so last among the useful ones
	}
	return 4
}

// orderRelations sorts a synset's edges most-informative-first, stably, so
// that any consumer taking the first N gets the N that matter.
func orderRelations(relations []wsd.Relation) []wsd.Relation {
	if len(relations) < 2 {
		return relations
	}
	sort.SliceStable(relations, func(a, b int) bool {
		return relationPriority(relations[a].Type) < relationPriority(relations[b].Type)
	})
	return relations
}

// cleanLemma strips BabelNet's Wikipedia-title decorations off a lemma.
//
// Senses that came from Wikipedia carry the article title verbatim, so a
// lemma arrives as "otter_(fishing_device)". The parenthetical is a
// disambiguator for human readers of Wikipedia, not part of the word — left
// in, it becomes a BM25 term no page will ever contain.
func cleanLemma(lemma string) string {
	s := strings.TrimSpace(lemma)
	if i := strings.Index(s, "_("); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// orderSenses puts WordNet-derived senses first, keeping the relative order
// within each group.
//
// BabelNet does NOT return senses in frequency order, and the difference is
// not cosmetic: "otter" returns 25 senses of which the animal is the fifth,
// behind a fishing device, a heraldic charge, a steamship and a town in
// Montana. Truncating to the first few candidates would discard the only
// sense anybody meant.
//
// Preferring WordNet-derived senses fixes that, and is principled rather than
// a hack: those are the lexicalised dictionary senses, and they are also the
// only ones with a position in the taxonomy, so they are the only ones that
// can contribute to semantic similarity at all. A term whose senses are ALL
// Wikipedia-only — a brand or a person — keeps them, because there is nothing
// better to keep.
func orderSenses(senses []wsd.Synset) []wsd.Synset {
	if len(senses) < 2 {
		return senses
	}
	out := make([]wsd.Synset, 0, len(senses))
	for _, s := range senses {
		if s.WordNetKey != "" {
			out = append(out, s)
		}
	}
	for _, s := range senses {
		if s.WordNetKey == "" {
			out = append(out, s)
		}
	}
	return out
}

// posFromID reads the part of speech off a BabelNet id ("bn:00008364n").
func posFromID(id string) string {
	if id == "" {
		return ""
	}
	last := id[len(id)-1:]
	switch last {
	case wsd.POSNoun, wsd.POSVerb, wsd.POSAdjective, wsd.POSAdverb:
		return last
	}
	return ""
}
