package analytics

import (
	"math"
	"testing"
)

func TestHistogramQuantiles(t *testing.T) {
	h := NewHistogram()
	for v := 1; v <= 1000; v++ {
		h.Observe(float64(v))
	}
	cases := []struct {
		q, want float64
	}{
		{0.50, 500},
		{0.95, 950},
		{0.99, 990},
	}
	for _, c := range cases {
		got := h.Quantile(c.q)
		relErr := math.Abs(got-c.want) / c.want
		t.Logf("p%-3.0f estimate=%.1f want≈%.0f relErr=%.4f", c.q*100, got, c.want, relErr)
		if relErr > 0.05 {
			t.Errorf("p%.0f: got %.1f want ~%.0f (relErr %.4f > 5%%)", c.q*100, got, c.want, relErr)
		}
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram()
	if h.Quantile(0.99) != 0 || h.Mean() != 0 {
		t.Errorf("empty histogram should report zeros")
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := NewHistogram()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Observe(float64(i%2000) / 10.0)
	}
}
