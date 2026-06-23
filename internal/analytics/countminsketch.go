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
type CountMinSketch struct {
	d      int
	w      uint64
	counts [][]uint64
	total  uint64
}

// NewCountMinSketch sizes the grid from the target error epsilon (relative to
// total volume) and failure probability delta: w = ceil(e/epsilon),
// d = ceil(ln(1/delta)).
func NewCountMinSketch(epsilon, delta float64) *CountMinSketch {
	w := uint64(math.Ceil(math.E / epsilon))
	if w < 1 {
		w = 1
	}
	d := int(math.Ceil(math.Log(1 / delta)))
	if d < 1 {
		d = 1
	}
	counts := make([][]uint64, d)
	for i := range counts {
		counts[i] = make([]uint64, w)
	}
	return &CountMinSketch{d: d, w: w, counts: counts}
}

// AddAndCount increments the key's counters and returns its post-increment
// estimate in a single pass.
func (c *CountMinSketch) AddAndCount(key string) uint64 {
	h := hash64(key)
	h1 := h & 0xffffffff
	h2 := (h >> 32) | 1 // odd, so the step never aliases within the row
	c.total++
	min := uint64(math.MaxUint64)
	for i := 0; i < c.d; i++ {
		pos := (h1 + uint64(i)*h2) % c.w
		c.counts[i][pos]++
		if v := c.counts[i][pos]; v < min {
			min = v
		}
	}
	return min
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
// frequency estimates while a small map holds the current leaders; a key that
// outscores the weakest leader evicts it. This is the standard CMS + heap
// heavy-hitter scheme, trading exact recall for O(1) memory in k.
type TopK struct {
	cms  *CountMinSketch
	k    int
	keep map[string]uint64
}

// NewTopK returns a tracker for the k heaviest keys, backed by a sketch sized
// from epsilon/delta.
func NewTopK(k int, epsilon, delta float64) *TopK {
	if k < 1 {
		k = 1
	}
	return &TopK{cms: NewCountMinSketch(epsilon, delta), k: k, keep: make(map[string]uint64, k+1)}
}

// Add records one occurrence of key and updates the leader set.
func (t *TopK) Add(key string) {
	est := t.cms.AddAndCount(key)
	if _, ok := t.keep[key]; ok {
		t.keep[key] = est
		return
	}
	if len(t.keep) < t.k {
		t.keep[key] = est
		return
	}
	minKey, minVal := "", uint64(math.MaxUint64)
	for kk, vv := range t.keep {
		if vv < minVal {
			minKey, minVal = kk, vv
		}
	}
	if est > minVal {
		delete(t.keep, minKey)
		t.keep[key] = est
	}
}

// Top returns the tracked leaders sorted by current (freshly queried) estimate,
// highest first, ties broken by key for stable output.
func (t *TopK) Top() []KeyCount {
	out := make([]KeyCount, 0, len(t.keep))
	for k := range t.keep {
		out = append(out, KeyCount{Key: k, Count: t.cms.Count(k)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// SizeBytes is the sketch footprint; the leader map is bounded by k.
func (t *TopK) SizeBytes() int { return t.cms.SizeBytes() }

// Reset clears the leader set and the underlying sketch for a new window.
func (t *TopK) Reset() {
	t.cms.Reset()
	clear(t.keep)
}
