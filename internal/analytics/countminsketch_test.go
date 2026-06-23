package analytics

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestCountMinNeverUndercounts(t *testing.T) {
	cms := NewCountMinSketch(0.001, 0.001)
	truth := map[string]uint64{}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		key := fmt.Sprintf("/p/%d", r.Intn(500))
		cms.AddAndCount(key)
		truth[key]++
	}
	for key, want := range truth {
		if got := cms.Count(key); got < want {
			t.Fatalf("undercount for %s: got %d want >= %d", key, got, want)
		}
	}
}

func TestTopKRecoversHeavyHitters(t *testing.T) {
	tk := NewTopK(10, 0.0005, 0.001)
	hot := map[string]bool{}
	for i := 0; i < 10; i++ {
		hot[fmt.Sprintf("/hot/%d", i)] = true
	}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 500000; i++ {
		if r.Float64() < 0.6 {
			tk.Add(fmt.Sprintf("/hot/%d", r.Intn(10))) // 60% of traffic on 10 paths
		} else {
			tk.Add(fmt.Sprintf("/cold/%d", r.Intn(50000))) // long tail
		}
	}
	top := tk.Top()
	if len(top) != 10 {
		t.Fatalf("expected 10 leaders, got %d", len(top))
	}
	for _, kc := range top {
		if !hot[kc.Key] {
			t.Errorf("non-hot key in top-10: %s (%d)", kc.Key, kc.Count)
		}
	}
	t.Logf("top path: %s with ~%d hits", top[0].Key, top[0].Count)
}

func TestCountMinSketchMerge(t *testing.T) {
	a, b := NewCountMinSketch(0.001, 0.001), NewCountMinSketch(0.001, 0.001)
	r := rand.New(rand.NewSource(3))
	truth := map[string]uint64{}
	for i := 0; i < 100000; i++ {
		key := fmt.Sprintf("/p/%d", r.Intn(500))
		if i%2 == 0 { // split the stream across two "instances"
			a.AddAndCount(key)
		} else {
			b.AddAndCount(key)
		}
		truth[key]++
	}
	a.Merge(b)
	for key, want := range truth { // a merged view never undercounts the combined stream
		if got := a.Count(key); got < want {
			t.Fatalf("merged sketch undercounts %s: got %d want >= %d", key, got, want)
		}
	}
}

func TestCountMinSketchReset(t *testing.T) {
	c := NewCountMinSketch(0.01, 0.01)
	c.AddAndCount("x")
	c.Reset()
	if got := c.Count("x"); got != 0 {
		t.Errorf("after reset Count(x) = %d, want 0", got)
	}
}

func TestTopKReset(t *testing.T) {
	tk := NewTopK(5, 0.001, 0.001)
	for i := 0; i < 100; i++ {
		tk.Add("/a")
	}
	tk.Reset()
	if top := tk.Top(); len(top) != 0 {
		t.Errorf("after reset Top() = %v, want empty", top)
	}
}

func BenchmarkTopKAdd(b *testing.B) {
	tk := NewTopK(10, 0.0005, 0.001)
	paths := make([]string, 1024)
	for i := range paths {
		paths[i] = fmt.Sprintf("/api/resource/%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tk.Add(paths[i%len(paths)])
	}
}
