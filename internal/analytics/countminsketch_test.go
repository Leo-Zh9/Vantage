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

// TestTopKHeapKeepsTrueLeaders checks the heap bookkeeping with a large k: the
// leaders must be exactly the keys with the highest true counts when those
// counts are well separated.
func TestTopKHeapKeepsTrueLeaders(t *testing.T) {
	const (
		keys = 300
		k    = 50
	)
	tk := NewTopK(k, 0.0005, 0.001)
	names := make([]string, keys)
	var stream []int
	for i := range names {
		names[i] = fmt.Sprintf("/k/%d", i)
		for n := 0; n < 20*(i+1); n++ { // key i gets 20*(i+1) hits
			stream = append(stream, i)
		}
	}
	r := rand.New(rand.NewSource(11))
	r.Shuffle(len(stream), func(a, b int) { stream[a], stream[b] = stream[b], stream[a] })
	for _, i := range stream {
		tk.Add(names[i])
	}
	top := tk.Top()
	if len(top) != k {
		t.Fatalf("got %d leaders, want %d", len(top), k)
	}
	want := map[string]bool{}
	for i := keys - k; i < keys; i++ {
		want[names[i]] = true
	}
	missing := 0
	for _, kc := range top {
		if !want[kc.Key] {
			missing++
		}
	}
	// Keys right at the cut-off differ by under 1% in count, so allow a little
	// slack for the sketch's overcount; the bulk must be exact.
	if missing > k/10 {
		t.Errorf("%d of the top %d are not true leaders", missing, k)
	}
	for i := 1; i < len(top); i++ {
		if top[i].Count > top[i-1].Count {
			t.Fatalf("Top() not sorted at %d", i)
		}
	}
	for key, i := range tk.idx { // heap index must stay consistent
		if tk.heap[i].Key != key {
			t.Fatalf("idx[%q]=%d but heap[%d]=%q", key, i, i, tk.heap[i].Key)
		}
	}
}

func BenchmarkTopKAddLargeK(b *testing.B) {
	tk := NewTopK(1024, 0.001, 0.001)
	keys := make([]string, 1<<16)
	for i := range keys {
		keys[i] = fmt.Sprintf("10.0.%d.%d", i>>8&255, i&255)
	}
	for i := 0; i < 1<<16; i++ { // fill the heap first, so misses dominate
		tk.Add(keys[i%len(keys)])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tk.Add(keys[i%len(keys)])
	}
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
