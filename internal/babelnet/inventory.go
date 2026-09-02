package babelnet

import (
	"context"
	"encoding/json"
	"errors"

	"enact/internal/logging"
	"enact/internal/opensearch"
	"enact/internal/wsd"
)

// Inventory is the BabelNet-backed wsd.Inventory: cache first, budget second,
// network last.
type Inventory struct {
	client *client
	cache  *cache
	budget *budget
	lang   string
	// maxSenses caps resolved candidates per lemma; see Config.MaxSenses.
	maxSenses int
	logger    *logging.Logger
}

// New builds the inventory. It does not contact BabelNet.
func New(cfg Config, osClient *opensearch.Client, logger *logging.Logger) *Inventory {
	return &Inventory{
		client:    newClient(cfg),
		cache:     newCache(osClient, cfg.CacheIndex),
		budget:    newBudget(osClient, cfg.CacheIndex, cfg.DailyBudget),
		lang:      cfg.SearchLang,
		maxSenses: cfg.MaxSenses,
		logger:    logger,
	}
}

// EnsureIndex verifies the cache index exists.
func (i *Inventory) EnsureIndex(ctx context.Context) error { return i.cache.EnsureIndex(ctx) }

// Spent and Remaining report the day's budget, for run reports and logs.
func (i *Inventory) Spent() int     { return i.budget.Spent() }
func (i *Inventory) Remaining() int { return i.budget.Remaining() }

// Flush persists the budget counter.
func (i *Inventory) Flush(ctx context.Context) { i.budget.Flush(ctx) }

// fetch is the cache-then-budget-then-network path shared by every lookup.
//
// The ordering is the whole design: a cached answer costs no budget, so a
// warm deployment can crawl all day on an allowance that would not cover one
// cold page.
func (i *Inventory) fetch(ctx context.Context, kind, key string, call func() (any, error)) (json.RawMessage, error) {
	if payload, ok := i.cache.get(ctx, kind, key); ok {
		return payload, nil
	}
	if err := i.budget.reserve(ctx); err != nil {
		i.budget.Flush(ctx)
		return nil, err
	}
	value, err := call()
	if err != nil {
		return nil, i.classify(ctx, err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	i.cache.put(ctx, kind, key, payload)
	return payload, nil
}

// classify turns the client's refusal into the more specific of the two
// errors it could mean.
//
// The signal is how much of the day has been spent. A refusal on the very
// first request of the day, when the counter says nothing has been used, is
// far more likely to be a bad key than an exhausted allowance — the allowance
// cannot have been exhausted by requests this deployment never made. Later in
// the day the counter is real evidence and exhaustion is the better guess.
//
// It is a heuristic and it can be wrong: another process may share the key
// and have spent it. Both errors stop the crawl identically, so being wrong
// costs a misleading sentence in the report, not incorrect behaviour.
func (i *Inventory) classify(ctx context.Context, err error) error {
	if !errors.Is(err, errRefused) {
		return err
	}
	i.budget.Flush(ctx)
	// The failed request was itself reserved, so a fresh day reads as 1.
	if i.budget.Spent() <= 1 {
		return ErrKeyRejected
	}
	return ErrBudgetExhausted
}

// Senses returns the candidate senses of a lemma.
//
// Each candidate is returned with its gloss and relations, which is what
// extended Lesk needs — and each of those is itself cached, so a second word
// sharing a sense pays nothing.
func (i *Inventory) Senses(ctx context.Context, lemma, pos string) ([]wsd.Synset, error) {
	ids, err := i.senseIDs(ctx, lemma, pos)
	if err != nil {
		return nil, err
	}
	// Resolve at most MaxSenses of them. Applied to the IDS, before any
	// synset is fetched, because that is where the cost is.
	if i.maxSenses > 0 && len(ids) > i.maxSenses {
		ids = ids[:i.maxSenses]
	}
	out := make([]wsd.Synset, 0, len(ids))
	for _, id := range ids {
		synset, err := i.Synset(ctx, id.ID)
		if err != nil {
			// Partial results are useful: Lesk can choose among the senses it
			// did resolve. Returning what we have alongside the error lets
			// the caller decide, and the budget error stops the run cleanly.
			return orderSenses(out), err
		}
		if synset.ID != "" {
			out = append(out, synset)
		}
	}
	return orderSenses(out), nil
}

// senseIDs finds a lemma's candidate senses, mixing dictionary senses with
// the encyclopaedic ones.
//
// Both lists are fetched and INTERLEAVED, rather than preferring WordNet and
// falling back. Preferring WordNet was the original design and it was wrong in
// exactly the case BabelNet exists to serve: "index" has 6 WordNet senses and
// 33 in BabelNet, and the 27 extra are where "database index" lives. Because
// the 6 existed, the fallback never fired and the technical sense was never
// a candidate — so a query about "opensearch indices" was read as being about
// semiotics, and the crawl went looking for philosophy.
//
// Interleaving keeps what the preference was for. WordNet senses are ordered
// by corpus frequency and lead, so an ordinary word still offers its ordinary
// meanings first; but the candidate list a bounded Lesk sees now contains
// both kinds, and the context decides. A term with no WordNet sense at all
// still resolves entirely from the encyclopaedic side.
//
// The cost is one extra getSynsetIds per lemma — cached forever, and small
// beside the per-sense lookups that MaxSenses already caps.
func (i *Inventory) senseIDs(ctx context.Context, lemma, pos string) ([]synsetIDResponse, error) {
	dictionary, err := i.senseIDsFrom(ctx, lemma, pos, sourceWordNet)
	if err != nil {
		return nil, err
	}
	all, err := i.senseIDsFrom(ctx, lemma, pos, "")
	if err != nil {
		// The dictionary senses alone are a usable, smaller candidate set.
		return dictionary, err
	}
	return interleaveSenses(dictionary, all), nil
}

// interleaveSenses alternates dictionary senses with encyclopaedic ones,
// preserving each side's own order and dropping duplicates.
//
// Alternating rather than concatenating matters because the caller truncates:
// with MaxSenses of 4, appending would give four dictionary senses and no
// technical one for any word common enough to have four.
func interleaveSenses(dictionary, all []synsetIDResponse) []synsetIDResponse {
	inDictionary := make(map[string]bool, len(dictionary))
	for _, s := range dictionary {
		inDictionary[s.ID] = true
	}
	encyclopaedic := make([]synsetIDResponse, 0, len(all))
	for _, s := range all {
		if !inDictionary[s.ID] {
			encyclopaedic = append(encyclopaedic, s)
		}
	}
	out := make([]synsetIDResponse, 0, len(dictionary)+len(encyclopaedic))
	for n := 0; n < len(dictionary) || n < len(encyclopaedic); n++ {
		if n < len(dictionary) {
			out = append(out, dictionary[n])
		}
		if n < len(encyclopaedic) {
			out = append(out, encyclopaedic[n])
		}
	}
	return out
}

func (i *Inventory) senseIDsFrom(ctx context.Context, lemma, pos, source string) ([]synsetIDResponse, error) {
	key := lemma + "|" + pos + "|" + i.lang + "|" + source
	payload, err := i.fetch(ctx, kindSenseIDs, key, func() (any, error) {
		return i.client.synsetIDs(ctx, lemma, pos, source)
	})
	if err != nil {
		return nil, err
	}
	var ids []synsetIDResponse
	if err := json.Unmarshal(payload, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// Synset resolves one sense with its relations — two API calls when cold,
// none when warm.
func (i *Inventory) Synset(ctx context.Context, id string) (wsd.Synset, error) {
	base, err := i.synsetBase(ctx, id)
	if err != nil {
		return wsd.Synset{}, err
	}
	payload, err := i.fetch(ctx, kindEdges, id, func() (any, error) {
		return i.client.edges(ctx, id)
	})
	if err != nil {
		// The definition without its edges is still usable for gloss
		// overlap; the caller sees the error and stops, but what it already
		// holds is not wrong.
		return toSynset(id, base, nil, i.lang), err
	}
	var edges []edgeResponse
	if err := json.Unmarshal(payload, &edges); err != nil {
		return toSynset(id, base, nil, i.lang), err
	}
	return toSynset(id, base, edges, i.lang), nil
}

// SynsetGloss resolves one sense WITHOUT its relations, for callers that only
// need the definition (wsd.GlossProvider).
//
// This halves the cost of extended Lesk's neighbour lookups, which are the
// single largest consumer of the daily budget.
func (i *Inventory) SynsetGloss(ctx context.Context, id string) (wsd.Synset, error) {
	base, err := i.synsetBase(ctx, id)
	if err != nil {
		return wsd.Synset{}, err
	}
	return toSynset(id, base, nil, i.lang), nil
}

func (i *Inventory) synsetBase(ctx context.Context, id string) (synsetResponse, error) {
	var base synsetResponse
	payload, err := i.fetch(ctx, kindSynset, id, func() (any, error) {
		return i.client.synset(ctx, id)
	})
	if err != nil {
		return base, err
	}
	err = json.Unmarshal(payload, &base)
	return base, err
}
