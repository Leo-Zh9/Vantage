package analytics

import (
	"fmt"
	"testing"
	"time"
)

func TestEngineAggregates(t *testing.T) {
	e := NewEngine(16384) // larger than the burst below, so nothing is dropped
	go e.Run()
	defer e.Stop()

	for i := 0; i < 10000; i++ {
		e.Observe(Event{
			IP:      fmt.Sprintf("198.51.100.%d", i%250),
			Path:    "/api/health",
			Status:  200,
			Latency: 2 * time.Millisecond,
			Bytes:   512,
		})
	}
	// Wait for the buffer to drain and a publish tick to land.
	waitFor(t, func() bool { return e.Snapshot().TotalRequests == 10000 })

	s := e.Snapshot()
	if s.TotalRequests != 10000 {
		t.Fatalf("total requests = %d, want 10000", s.TotalRequests)
	}
	if s.UniqueVisitors < 240 || s.UniqueVisitors > 260 {
		t.Errorf("unique visitors = %d, want ~250", s.UniqueVisitors)
	}
	if s.TopPaths[0].Key != "/api/health" {
		t.Errorf("top path = %q, want /api/health", s.TopPaths[0].Key)
	}
	if s.StatusClasses["2xx"] != 10000 {
		t.Errorf("2xx = %d, want 10000", s.StatusClasses["2xx"])
	}
	if s.MemoryBytes <= 0 {
		t.Errorf("expected non-zero fixed sketch memory")
	}
}

func TestObserveDropsWhenBufferFull(t *testing.T) {
	e := NewEngine(8) // run loop intentionally not started, so nothing drains
	for i := 0; i < 1000; i++ {
		e.Observe(Event{IP: "203.0.113.1", Path: "/", Status: 200})
	}
	if dropped := e.dropped.Load(); dropped == 0 {
		t.Fatalf("expected drops when buffer is full and undrained, got 0")
	} else {
		t.Logf("dropped %d/1000 events under a full buffer (backpressure works)", dropped)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func BenchmarkEngineObserve(b *testing.B) {
	e := NewEngine(1 << 16)
	go e.Run()
	defer e.Stop()
	ev := Event{IP: "203.0.113.9", Path: "/api/v1/items", Status: 200, Latency: time.Millisecond, Bytes: 256}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Observe(ev)
	}
}
