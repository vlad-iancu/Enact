package crawler

import (
	"container/heap"

	"enact/internal/source"
)

// Candidate is a reference waiting in the frontier.
//
// An alias rather than a type of its own now that references are the currency:
// the frontier orders things it has not looked at, and that is exactly what a
// source.Reference is. Keeping the name means the persisted frontier of a
// paused run still deserialises.
type Candidate = source.Reference

// Frontier is a best-first queue of unvisited links.
//
// This is what makes the crawler focused. A breadth-first crawler fetches
// every link at depth 1 before any at depth 2, so a budget of 200 pages spent
// on a large site never leaves the front page's neighbourhood. A best-first
// frontier always takes the most promising link anywhere it has seen, so the
// budget follows the topic rather than the site's shape — and because
// priority comes from the parent page's score, a relevant page's links are
// explored before an irrelevant one's.
//
// Not safe for concurrent use; the crawl loop owns it.
type Frontier struct {
	heap  candidateHeap
	seen  map[string]bool
	limit int
}

// DefaultFrontierLimit caps how many candidates are held at once.
//
// A frontier is not free: it is memory, and it is serialised into the run
// report when a crawl pauses. A large site can offer tens of thousands of
// links long before the page budget runs out, and the tail of that queue is
// never going to be reached anyway — the crawl stops at the score threshold
// or the page cap first.
const DefaultFrontierLimit = 10000

// NewFrontier returns an empty frontier.
func NewFrontier(limit int) *Frontier {
	if limit <= 0 {
		limit = DefaultFrontierLimit
	}
	f := &Frontier{heap: newCandidateHeap(), seen: map[string]bool{}, limit: limit}
	heap.Init(&f.heap)
	return f
}

// Push adds a candidate, or raises one already queued, reporting whether the
// frontier changed.
//
// A page is often linked from several places, and the sightings are not equally
// informative: priority is `0.6 * parent score + 0.4 * hint`, so the same URL
// discovered from a page that scored 0.05 and from one that scored 0.8 arrives
// with wildly different priorities. Taking the first sighting and ignoring the
// rest means the ordering records WHERE A LINK WAS FIRST FOUND rather than the
// best evidence for it — and the first sighting is systematically the weaker
// one, because the crawl reaches hub and index pages early and they score low
// on prose while linking to everything.
//
// So a better sighting wins: the queued entry takes the new score, depth and
// hint, and is fixed in place. Worse sightings are ignored, which keeps the
// priority monotonic and the run reproducible — the order in which two equally
// good routes were discovered cannot change the outcome.
//
// A candidate already RETRIEVED is not queued again at any score. That is what
// stops the crawl cycling between two pages that link to each other.
func (f *Frontier) Push(c Candidate) bool {
	if c.ID == "" {
		return false
	}
	// Still queued: this is a second route to somewhere not yet visited, so
	// there is a decision to make rather than a duplicate to discard.
	if i, queued := f.heap.index[c.ID]; queued {
		existing := f.heap.items[i]
		if c.Score <= existing.Score {
			return false
		}
		existing.Score = c.Score
		// Depth and hint travel with the score: they describe the route being
		// credited, and a report that showed a raised score beside the depth
		// of the route it rejected would be lying about how it got there.
		existing.Depth = c.Depth
		if c.Hint != "" {
			existing.Hint = c.Hint
		}
		heap.Fix(&f.heap, i)
		return true
	}
	if f.seen[c.ID] {
		return false
	}
	f.seen[c.ID] = true
	heap.Push(&f.heap, &c)
	// Over the limit, drop the worst candidate rather than refusing the new
	// one: a late-discovered but highly relevant link should displace an
	// early mediocre one.
	for f.heap.Len() > f.limit {
		f.dropWorst()
	}
	return true
}

// Requeue puts a popped candidate back, for a run that is pausing rather than
// finishing with it.
//
// Needed because Push refuses anything already seen, which is right for a link
// encountered again and wrong for the one page the crawl was holding when its
// allowance ran out: that page was never retrieved, and without this it is
// dropped from the persisted frontier and never retried on the next run.
func (f *Frontier) Requeue(c Candidate) bool {
	delete(f.seen, c.ID)
	return f.Push(c)
}

// dropWorst removes the lowest-scoring candidate. The heap is ordered for
// max-extraction, so the minimum is somewhere in the leaves; a linear scan is
// acceptable because this runs only when the frontier is full.
func (f *Frontier) dropWorst() {
	if f.heap.Len() == 0 {
		return
	}
	worst := 0
	for i := 1; i < f.heap.Len(); i++ {
		if f.heap.items[i].Score < f.heap.items[worst].Score {
			worst = i
		}
	}
	// Evicted for want of room, not judged — so it is allowed back if a later,
	// better route finds it. Leaving it in seen would make a capacity
	// accident permanent.
	delete(f.seen, f.heap.items[worst].ID)
	heap.Remove(&f.heap, worst)
}

// Pop removes and returns the highest-scoring candidate.
func (f *Frontier) Pop() (Candidate, bool) {
	if f.heap.Len() == 0 {
		return Candidate{}, false
	}
	c := heap.Pop(&f.heap).(*Candidate)
	return *c, true
}

// Peek reports the best score currently queued, without removing it. The
// crawl loop uses it to decide whether anything left is worth fetching.
func (f *Frontier) Peek() (Candidate, bool) {
	if f.heap.Len() == 0 {
		return Candidate{}, false
	}
	return *f.heap.items[0], true
}

// Len is how many candidates are queued.
func (f *Frontier) Len() int { return f.heap.Len() }

// Seen reports whether a URL has ever entered the frontier.
func (f *Frontier) Seen(url string) bool { return f.seen[url] }

// Remaining returns every queued candidate, best first, without draining the
// frontier.
//
// This is how a paused run is resumed: the candidates go into the run record,
// and the next run pushes them back. Without it, a crawl stopped by the
// BabelNet budget would restart from its seed every day and never get further
// than the first day's reach.
func (f *Frontier) Remaining() []Candidate {
	out := make([]Candidate, 0, f.heap.Len())
	// Copy the pointers, then sort by score rather than mutating the heap.
	tmp := newCandidateHeap()
	tmp.items = make([]*Candidate, len(f.heap.items))
	copy(tmp.items, f.heap.items)
	for i, c := range tmp.items {
		tmp.index[c.ID] = i
	}
	clone := &tmp
	heap.Init(clone)
	for clone.Len() > 0 {
		c := heap.Pop(clone).(*Candidate)
		out = append(out, *c)
	}
	return out
}

// candidateHeap is a max-heap on Score.
//
// It carries an ID-to-position index, maintained by Swap, Push and Pop, so
// that a re-sighting can find its entry in constant time and heap.Fix it.
// Without the index, raising a candidate would mean a linear scan on every
// link of every page — the one operation a crawl does most.
type candidateHeap struct {
	items []*Candidate
	index map[string]int
}

func newCandidateHeap() candidateHeap {
	return candidateHeap{index: map[string]int{}}
}

func (h candidateHeap) Len() int { return len(h.items) }

func (h candidateHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if a.Score != b.Score {
		return a.Score > b.Score // max-heap
	}
	// Ties go to the shallower candidate, which keeps the crawl closer to the
	// seed when it has no reason to prefer either, and makes the order
	// deterministic for a reproducible report.
	if a.Depth != b.Depth {
		return a.Depth < b.Depth
	}
	return a.ID < b.ID
}

func (h candidateHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.index[h.items[i].ID] = i
	h.index[h.items[j].ID] = j
}

func (h *candidateHeap) Push(x any) {
	c := x.(*Candidate)
	h.index[c.ID] = len(h.items)
	h.items = append(h.items, c)
}

func (h *candidateHeap) Pop() any {
	old := h.items
	n := len(old)
	c := old[n-1]
	old[n-1] = nil
	h.items = old[:n-1]
	delete(h.index, c.ID)
	return c
}
