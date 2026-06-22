package analytics

import (
	"fmt"
	"math"
	"testing"
)

func TestHyperLogLogAccuracy(t *testing.T) {
	for _, n := range []int{1000, 10000, 100000, 1000000} {
		h := NewHyperLogLog(14)
		for i := 0; i < n; i++ {
			h.Add(fmt.Sprintf("visitor-%d", i))
		}
		est := h.Estimate()
		relErr := math.Abs(est-float64(n)) / float64(n)
		t.Logf("n=%-8d estimate=%-12.0f relErr=%.4f mem=%dB", n, est, relErr, h.SizeBytes())
		if relErr > 0.02 {
			t.Errorf("n=%d: relative error %.4f exceeds 2%%", n, relErr)
		}
	}
}

func TestHyperLogLogIgnoresDuplicates(t *testing.T) {
	h := NewHyperLogLog(14)
	for round := 0; round < 5; round++ {
		for i := 0; i < 5000; i++ {
			h.Add(fmt.Sprintf("ip-%d", i))
		}
	}
	est := h.Estimate()
	if relErr := math.Abs(est-5000) / 5000; relErr > 0.02 {
		t.Errorf("duplicates inflated estimate: %.0f (relErr %.4f)", est, relErr)
	}
}

func TestHyperLogLogMerge(t *testing.T) {
	a, b := NewHyperLogLog(14), NewHyperLogLog(14)
	for i := 0; i < 30000; i++ {
		a.Add(fmt.Sprintf("a-%d", i))
	}
	for i := 0; i < 30000; i++ {
		b.Add(fmt.Sprintf("b-%d", i))
	}
	a.Merge(b)
	if relErr := math.Abs(a.Estimate()-60000) / 60000; relErr > 0.02 {
		t.Errorf("merged estimate %.0f, relErr %.4f", a.Estimate(), relErr)
	}
}

func TestHyperLogLogMemoryIsFixed(t *testing.T) {
	h := NewHyperLogLog(14)
	before := h.SizeBytes()
	for i := 0; i < 2000000; i++ {
		h.Add(fmt.Sprintf("k-%d", i))
	}
	if h.SizeBytes() != before || before != 16384 {
		t.Errorf("expected fixed 16384B, got before=%d after=%d", before, h.SizeBytes())
	}
}

func BenchmarkHyperLogLogAdd(b *testing.B) {
	h := NewHyperLogLog(14)
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("203.0.113.%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Add(keys[i%len(keys)])
	}
}
