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

func TestEngineRateLimitBlocksHeavyIP(t *testing.T) {
	e := NewEngine(1 << 16)
	e.EnableRateLimit(100, 2*time.Second) // >100 req/IP within a 2s window
	go e.Run()
	defer e.Stop()

	for i := 0; i < 500; i++ {
		e.Observe(Event{IP: "203.0.113.50", Path: "/", Status: 200})
	}
	for i := 0; i < 10; i++ {
		e.Observe(Event{IP: "198.51.100.7", Path: "/", Status: 200})
	}

	// The blocked set is published on the next tick.
	waitFor(t, func() bool { return e.Blocked("203.0.113.50") })
	if e.Blocked("198.51.100.7") {
		t.Errorf("light IP (10 reqs) should not be blocked at threshold 100")
	}
}

func TestEngineSiteAnalytics(t *testing.T) {
	e := NewEngine(1 << 12)
	go e.Run()
	defer e.Stop()

	site := "www.leo-zhang.com"
	page := func(ip, path, ref string) Event {
		return Event{IP: ip, Method: "GET", Host: site, Path: path, UserAgent: chromeUA, Referer: ref, Status: 200}
	}
	events := []Event{
		page("198.51.100.1", "/", "https://www.linkedin.com/feed/"),
		page("198.51.100.1", "/projects", "https://www.leo-zhang.com/"), // internal click: no referrer
		page("198.51.100.2", "/", "https://github.com/Leo-Zh9"),
		page("198.51.100.2", "/_next/static/chunks/app.js", ""), // asset, not a page view
		{IP: "198.51.100.2", Method: "GET", Host: site, Path: "/about", UserAgent: chromeUA, Status: 200, Prefetch: true},
		{IP: "198.51.100.3", Method: "GET", Host: site, Path: "/missing", UserAgent: chromeUA, Status: 404},
		{IP: "66.249.66.1", Method: "GET", Host: site, Path: "/", UserAgent: "Googlebot/2.1", Status: 200},
		{IP: "45.0.0.9", Method: "GET", Host: site, Path: "/wp-login.php", UserAgent: chromeUA, Status: 404},
		{IP: "45.0.0.9", Method: "GET", Host: site, Path: "/.env", UserAgent: "", Status: 404},
	}
	for _, ev := range events {
		e.Observe(ev)
	}
	waitFor(t, func() bool { return e.Snapshot().TotalRequests == uint64(len(events)) })
	s := e.Snapshot()

	if s.PageViews != 3 {
		t.Errorf("page views = %d, want 3 (/, /projects, /)", s.PageViews)
	}
	if s.HumanVisitors != 3 {
		t.Errorf("human visitors = %d, want 3", s.HumanVisitors)
	}
	if s.BotRequests != 1 || s.ScannerProbes != 2 {
		t.Errorf("bots=%d scanners=%d, want 1 and 2", s.BotRequests, s.ScannerProbes)
	}
	if len(s.TopPages) == 0 || s.TopPages[0].Key != "/" || s.TopPages[0].Count != 2 {
		t.Errorf("top pages = %+v, want / with 2 views first", s.TopPages)
	}
	refs := map[string]uint64{}
	for _, kc := range s.TopReferrers {
		refs[kc.Key] = kc.Count
	}
	if len(refs) != 2 || refs["linkedin.com"] != 1 || refs["github.com"] != 1 {
		t.Errorf("referrers = %+v, want linkedin.com and github.com once each", s.TopReferrers)
	}
	if len(s.TopProbes) != 2 {
		t.Errorf("probes = %+v, want the two scanner paths", s.TopProbes)
	}
	if s.Today.PageViews != 3 || s.Today.ScannerProbes != 2 || s.Today.Visitors != 3 {
		t.Errorf("today = %+v", s.Today)
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
