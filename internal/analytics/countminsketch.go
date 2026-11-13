package analytics

import (
	"math"
	"sort"
)

// KeyCount is a key paired with its estimated frequency. It is the unit the
// dashboard and JSON API use for "top paths" and "top IPs".
type KeyCount struct {
	Key   string `json:"key"`
	Count uint64 `json:"count"`
}

// CountMinSketch estimates how often each key appears in a stream using a fixed
// d×w grid of counters. It never undercounts; with probability 1-delta the
// overcount is at most epsilon*N where N is the total number of increments. The
// d row hashes are derived from one 64-bit hash by double hashing
// (Kirsch–Mitzenmacher), so a lookup costs one hash regardless of depth.
//
// Updates are conservative: only the cells holding the key's current minimum
// are raised. That keeps the never-undercount guarantee but sharply cuts the
// overcount light keys pick up from heavy ones they collide with, which matters
// for the rate limiter (fewer innocent clients flagged during a flood).
type CountMinSketch struct {
	d      int
	w      uint64
	counts [][]uint64
	total  uint64
	pos    []uint64 // scratch for one update, reused so Add never allocates
}

// NewCountMinSketch sizes the grid from the target error epsilon (relative to
// total volume) and failure probability delta: w = ceil(e/epsilon),
// d = ceil(ln(1/delta)).
func NewCountMinSketch(epsilon, delta float64) *CountMinSketch {
	return NewCountMinSketchDims(int(math.Ceil(math.Log(1/delta))), int(math.Ceil(math.E/epsilon)))
}

// NewCountMinSketchDims builds a sketch with an explicit depth d and width w.
func NewCountMinSketchDims(d, w int) *CountMinSketch {
	if d < 1 {
		d = 1
	}
	if w < 1 {
		w = 1
	}
	counts := make([][]uint64, d)
	for i := range counts {
		counts[i] = make([]uint64, w)
	}
	return &CountMinSketch{d: d, w: uint64(w), counts: counts, pos: make([]uint64, d)}
}

// AddAndCount increments the key (conservative update) and returns its
// post-increment estimate in a single pass.
func (c *CountMinSketch) AddAndCount(key string) uint64 {
	h := hash64(key)
	h1 := h & 0xffffffff
	h2 := (h >> 32) | 1 // odd, so the step never aliases within the row
	c.total++
	min := uint64(math.MaxUint64)
	for i := 0; i < c.d; i++ {
		p := (h1 + uint64(i)*h2) % c.w
		c.pos[i] = p
		if v := c.counts[i][p]; v < min {
			min = v
		}
	}
	next := min + 1
	for i := 0; i < c.d; i++ {
		if row := c.counts[i]; row[c.pos[i]] < next {
			row[c.pos[i]] = next
		}
	}
	return next
}

// Count returns the current estimate for a key without modifying the sketch.
func (c *CountMinSketch) Count(key string) uint64 {
	h := hash64(key)
	h1 := h & 0xffffffff
	h2 := (h >> 32) | 1
	min := uint64(math.MaxUint64)
	for i := 0; i < c.d; i++ {
		pos := (h1 + uint64(i)*h2) % c.w
		if v := c.counts[i][pos]; v < min {
			min = v
		}
	}
	return min
}

// SizeBytes is the fixed counter footprint (8 bytes per cell).
func (c *CountMinSketch) SizeBytes() int { return c.d * int(c.w) * 8 }

// Reset zeroes every counter so the sketch can be reused for a new window.
func (c *CountMinSketch) Reset() {
	for i := range c.counts {
		clear(c.counts[i])
	}
	c.total = 0
}

// Merge folds another sketch of identical dimensions into c by summing counters
// cell-wise. The merged Count estimates match what one sketch would have produced
// over the combined stream, so per-instance sketches roll up into a global view.
func (c *CountMinSketch) Merge(o *CountMinSketch) {
	if o.d != c.d || o.w != c.w {
		return
	}
	for i := range c.counts {
		row, orow := c.counts[i], o.counts[i]
		for j := range row {
			row[j] += orow[j]
		}
	}
	c.total += o.total
}

// TopK tracks the k most frequent keys in a stream. A Count-Min Sketch supplies
// frequency estimates while a min-heap of size k holds the current leaders, so
// the weakest leader is always at the root: deciding whether a new key gets in
// is O(1) and swapping it in is O(log k). (An earlier version scanned every
// leader on each miss, which made a k=1024 tracker O(k) per request.)
type TopK struct {
	cms  *CountMinSketch
	k    int
	heap []KeyCount     // min-heap by Count
	idx  map[string]int // key -> position in heap
}

// NewTopK returns a tracker for the k heaviest keys, backed by a sketch sized
// from epsilon/delta.
func NewTopK(k int, epsilon, delta float64) *TopK {
	if k < 1 {
		k = 1
	}
	return &TopK{
		cms:  NewCountMinSketch(epsilon, delta),
		k:    k,
		heap: make([]KeyCount, 0, k),
		idx:  make(map[string]int, k),
	}
}

// Add records one occurrence of key and updates the leader set.
func (t *TopK) Add(key string) {
	est := t.cms.AddAndCount(key)
	if i, ok := t.idx[key]; ok {
		// Estimates only grow between resets, so a raised count can only need to
		// move away from the root.
		t.heap[i].Count = est
		t.down(i)
		return
	}
	if len(t.heap) < t.k {
		t.heap = append(t.heap, KeyCount{Key: key, Count: est})
		t.idx[key] = len(t.heap) - 1
		t.up(len(t.heap) - 1)
		return
	}
	if est > t.heap[0].Count {
		delete(t.idx, t.heap[0].Key)
		t.heap[0] = KeyCount{Key: key, Count: est}
		t.idx[key] = 0
		t.down(0)
	}
}

func (t *TopK) less(i, j int) bool { return t.heap[i].Count < t.heap[j].Count }

func (t *TopK) swap(i, j int) {
	t.heap[i], t.heap[j] = t.heap[j], t.heap[i]
	t.idx[t.heap[i].Key] = i
	t.idx[t.heap[j].Key] = j
}

func (t *TopK) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !t.less(i, parent) {
			return
		}
		t.swap(i, parent)
		i = parent
	}
}

func (t *TopK) down(i int) {
	n := len(t.heap)
	for {
		smallest, l, r := i, 2*i+1, 2*i+2
		if l < n && t.less(l, smallest) {
			smallest = l
		}
		if r < n && t.less(r, smallest) {
			smallest = r
		}
		if smallest == i {
			return
		}
		t.swap(i, smallest)
		i = smallest
	}
}

// Top returns the tracked leaders sorted by current (freshly queried) estimate,
// highest first, ties broken by key for stable output.
func (t *TopK) Top() []KeyCount {
	out := make([]KeyCount, 0, len(t.heap))
	for _, kc := range t.heap {
		out = append(out, KeyCount{Key: kc.Key, Count: t.cms.Count(kc.Key)})
	}
	sortKeyCounts(out)
	return out
}

// SizeBytes is the sketch footprint; the leader heap is bounded by k.
func (t *TopK) SizeBytes() int { return t.cms.SizeBytes() }

// Reset clears the leader set and the underlying sketch for a new window.
func (t *TopK) Reset() {
	t.cms.Reset()
	t.heap = t.heap[:0]
	clear(t.idx)
}

// sortKeyCounts orders by count descending, then key ascending for stable output.
func sortKeyCounts(out []KeyCount) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
}
