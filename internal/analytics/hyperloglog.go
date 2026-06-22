// Package analytics implements the constant-memory streaming estimators Vantage
// uses to summarize live traffic: cardinality (HyperLogLog), heavy hitters
// (Count-Min Sketch + top-K), and a latency distribution (bucketed histogram).
// None of them store per-item state, so memory stays flat no matter how much
// traffic flows through the proxy.
package analytics

import (
	"math"
	"math/bits"
)

// HyperLogLog estimates the number of distinct items in a stream using a fixed
// amount of memory. With precision p it keeps m = 2^p one-byte registers, so a
// p=14 sketch tracks unbounded cardinality in 16 KB with a standard error of
// about 1.04/sqrt(m) ≈ 0.81%.
type HyperLogLog struct {
	p         uint8
	m         uint32
	registers []uint8
}

// NewHyperLogLog returns a sketch with the given precision. p is clamped to
// [4, 16].
func NewHyperLogLog(p uint8) *HyperLogLog {
	if p < 4 {
		p = 4
	}
	if p > 16 {
		p = 16
	}
	m := uint32(1) << p
	return &HyperLogLog{p: p, m: m, registers: make([]uint8, m)}
}

// Add records one occurrence of an item. Adding the same item again does not
// change the estimate.
func (h *HyperLogLog) Add(s string) {
	x := hash64(s)
	idx := x >> (64 - uint(h.p)) // top p bits choose the register
	// The remaining 64-p bits decide the register value: the position of the
	// leftmost set bit, counted from the top, plus one. ORing the low p bits to
	// 1 caps the run at 64-p so an all-zero suffix can't report a 65th zero.
	rest := (x << uint(h.p)) | ((1 << uint(h.p)) - 1)
	rank := uint8(bits.LeadingZeros64(rest)) + 1
	if rank > h.registers[idx] {
		h.registers[idx] = rank
	}
}

// Estimate returns the approximate number of distinct items added.
func (h *HyperLogLog) Estimate() float64 {
	m := float64(h.m)
	sum := 0.0
	zeros := 0
	for _, r := range h.registers {
		sum += 1.0 / float64(uint64(1)<<r)
		if r == 0 {
			zeros++
		}
	}
	est := alpha(h.m) * m * m / sum
	// Linear counting is far more accurate than the raw estimator when the
	// sketch is sparse (many empty registers, i.e. low cardinality).
	if est <= 2.5*m && zeros > 0 {
		est = m * math.Log(m/float64(zeros))
	}
	return est
}

// Merge folds another sketch of equal precision into h by taking the
// register-wise maximum. This is how per-window sketches combine into a total.
func (h *HyperLogLog) Merge(o *HyperLogLog) {
	if o.p != h.p {
		return
	}
	for i, r := range o.registers {
		if r > h.registers[i] {
			h.registers[i] = r
		}
	}
}

// Reset clears every register so the sketch can be reused.
func (h *HyperLogLog) Reset() {
	for i := range h.registers {
		h.registers[i] = 0
	}
}

// SizeBytes is the fixed register footprint, independent of cardinality.
func (h *HyperLogLog) SizeBytes() int { return len(h.registers) }

func alpha(m uint32) float64 {
	switch m {
	case 16:
		return 0.673
	case 32:
		return 0.697
	case 64:
		return 0.709
	default:
		return 0.7213 / (1 + 1.079/float64(m))
	}
}
