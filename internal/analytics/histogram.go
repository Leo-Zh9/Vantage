package analytics

import "sort"

// Histogram summarizes a latency distribution in fixed exponential buckets so
// it can report percentiles in constant memory, with no per-sample storage.
// Bucket boundaries grow geometrically (~20% apart), which bounds the relative
// error of any reported quantile to roughly that step while keeping the bucket
// count small across microseconds-to-minutes latencies.
type Histogram struct {
	bounds []float64 // ascending upper bounds, in milliseconds
	counts []uint64  // len(bounds)+1; the last cell is the +Inf overflow
	total  uint64
	sum    float64
	min    float64
	max    float64
}

// NewHistogram builds buckets spanning ~50µs to ~60s.
func NewHistogram() *Histogram {
	var bounds []float64
	for v := 0.05; v < 60000; v *= 1.20 {
		bounds = append(bounds, v)
	}
	return &Histogram{
		bounds: bounds,
		counts: make([]uint64, len(bounds)+1),
	}
}

// Observe records one latency sample in milliseconds.
func (h *Histogram) Observe(ms float64) {
	if h.total == 0 || ms < h.min {
		h.min = ms
	}
	if ms > h.max {
		h.max = ms
	}
	h.total++
	h.sum += ms
	i := sort.SearchFloat64s(h.bounds, ms)
	h.counts[i]++
}

// Quantile estimates the q-th quantile (0 < q < 1) by walking cumulative bucket
// counts and linearly interpolating within the bucket that crosses the target
// rank. Results are clamped to the observed [min, max].
func (h *Histogram) Quantile(q float64) float64 {
	if h.total == 0 {
		return 0
	}
	target := q * float64(h.total)
	var cum uint64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		if float64(cum)+float64(c) < target {
			cum += c
			continue
		}
		lower := 0.0
		if i > 0 {
			lower = h.bounds[i-1]
		}
		upper := h.max
		if i < len(h.bounds) {
			upper = h.bounds[i]
		}
		frac := (target - float64(cum)) / float64(c)
		v := lower + frac*(upper-lower)
		return clamp(v, h.min, h.max)
	}
	return h.max
}

// Mean returns the average of all observed samples.
func (h *Histogram) Mean() float64 {
	if h.total == 0 {
		return 0
	}
	return h.sum / float64(h.total)
}

// Max returns the largest observed sample.
func (h *Histogram) Max() float64 { return h.max }

// Count returns the number of samples observed.
func (h *Histogram) Count() uint64 { return h.total }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
