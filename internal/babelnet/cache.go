package babelnet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"enact/internal/opensearch"
	"enact/internal/wsd"
)

// cacheEntry is one stored answer. Everything BabelNet returns is a fact
// about the language, so entries have no owner, no organization and no
// expiry — only a version, so a future BabelNet release can invalidate the
// lot by bumping it rather than by a migration.
type cacheEntry struct {
	Key       string          `json:"key"`
	Kind      string          `json:"kind"`
	Version   int             `json:"version"`
	Payload   json.RawMessage `json:"payload"`
	CachedAt  time.Time       `json:"cached_at"`
	SourceURL string          `json:"-"`
}

const (
	cacheVersion = 1
	kindSenseIDs = "ids"
	kindSynset   = "synset"
	kindEdges    = "edges"
)

// cache is a read-through store over OpenSearch with an in-process layer in
// front of it.
//
// The memory layer is not an optimisation detail — within a single page,
// extended Lesk asks for the same handful of common synsets over and over,
// and a network round trip to OpenSearch for each would dominate the run.
type cache struct {
	os    *opensearch.Client
	index string

	mu  sync.RWMutex
	mem map[string]json.RawMessage
}

func newCache(os *opensearch.Client, index string) *cache {
	return &cache{os: os, index: index, mem: map[string]json.RawMessage{}}
}

// EnsureIndex verifies the cache index exists.
//
// Unlike the platform's data indices this one is safe to lose: it is
// rebuildable from BabelNet, at the cost of the requests it was there to
// avoid. It is still required to exist, so that a misconfigured deployment
// fails at startup rather than silently spending its whole daily budget on
// re-fetching what it should have cached.
func (c *cache) EnsureIndex(ctx context.Context) error {
	exists, err := c.os.IndexExists(ctx, c.index)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("babelnet: required index %q is missing; run `make infrastructure-up` to create it", c.index)
	}
	return nil
}

func docID(kind, key string) string {
	return kind + ":" + strings.ToLower(key)
}

// get returns a cached payload, reporting whether it was found.
func (c *cache) get(ctx context.Context, kind, key string) (json.RawMessage, bool) {
	id := docID(kind, key)
	c.mu.RLock()
	payload, ok := c.mem[id]
	c.mu.RUnlock()
	if ok {
		return payload, true
	}
	// No persistent store: the in-process layer is the whole cache. Used by
	// tests, and the honest behaviour if a deployment ever runs without one.
	if c.os == nil {
		return nil, false
	}
	var entry cacheEntry
	found, err := c.os.GetSource(ctx, c.index, id, &entry)
	if err != nil || !found || entry.Version != cacheVersion {
		// A cache read failure is not worth surfacing: the only consequence
		// is a request that would otherwise have been avoided, and turning a
		// transient OpenSearch blip into a failed crawl would be worse.
		return nil, false
	}
	c.mu.Lock()
	c.mem[id] = entry.Payload
	c.mu.Unlock()
	return entry.Payload, true
}

// put stores a payload. A write failure is swallowed for the same reason a
// read failure is: it costs requests later, not correctness now.
func (c *cache) put(ctx context.Context, kind, key string, payload json.RawMessage) {
	id := docID(kind, key)
	c.mu.Lock()
	c.mem[id] = payload
	c.mu.Unlock()

	if c.os == nil {
		return
	}
	body, err := json.Marshal(cacheEntry{
		Key: key, Kind: kind, Version: cacheVersion,
		Payload: payload, CachedAt: time.Now().UTC(),
	})
	if err != nil {
		return
	}
	_ = c.os.IndexDoc(ctx, c.index, id, body)
}

// budget counts requests against a per-UTC-day allowance.
//
// Held in memory and persisted, rather than read-modify-written on every
// request: the platform runs one orchestrator (as its other sweeps already
// assume), and a counter that costs a round trip per increment would be a
// significant fraction of the work it is metering. The persisted value
// survives a restart, so a crash does not hand the day's budget back.
type budget struct {
	os    *opensearch.Client
	index string
	limit int

	mu    sync.Mutex
	day   string
	spent int
	dirty bool
}

func newBudget(os *opensearch.Client, index string, limit int) *budget {
	return &budget{os: os, index: index, limit: limit}
}

type budgetDoc struct {
	Day   string `json:"day"`
	Spent int    `json:"spent"`
}

func budgetDocID(day string) string { return "budget:" + day }

// today is the current UTC date. UTC rather than local time because that is
// when BabelNet resets, and a crawl that believed otherwise would stop early
// or overrun.
func today() string { return time.Now().UTC().Format("2006-01-02") }

// reserve claims one request, returning ErrBudgetExhausted when the day's
// allowance is spent.
func (b *budget) reserve(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	day := today()
	if b.day != day {
		b.day = day
		b.spent = b.load(ctx, day)
		b.dirty = false
	}
	if b.limit > 0 && b.spent >= b.limit {
		return ErrBudgetExhausted
	}
	b.spent++
	b.dirty = true
	return nil
}

// Spent reports the day's usage so far, for the run report.
func (b *budget) Spent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// Remaining reports how much of the day's allowance is left.
func (b *budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return 0
	}
	if left := b.limit - b.spent; left > 0 {
		return left
	}
	return 0
}

func (b *budget) load(ctx context.Context, day string) int {
	if b.os == nil {
		return 0
	}
	var doc budgetDoc
	found, err := b.os.GetSource(ctx, b.index, budgetDocID(day), &doc)
	if err != nil || !found || doc.Day != day {
		return 0
	}
	return doc.Spent
}

// Flush persists the counter. Called at the end of a run and whenever the
// budget is exhausted, not per request.
func (b *budget) Flush(ctx context.Context) {
	b.mu.Lock()
	day, spent, dirty := b.day, b.spent, b.dirty
	b.dirty = false
	b.mu.Unlock()
	if !dirty || day == "" || b.os == nil {
		return
	}
	body, err := json.Marshal(budgetDoc{Day: day, Spent: spent})
	if err != nil {
		return
	}
	_ = b.os.IndexDoc(ctx, b.index, budgetDocID(day), body)
}

// compile-time proof that the inventory satisfies both interfaces.
var (
	_ wsd.Inventory     = (*Inventory)(nil)
	_ wsd.GlossProvider = (*Inventory)(nil)
)
