package analytics

import (
	"fmt"
	"testing"
	"time"
)

// fakeClock lets tests move time forward without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(threshold uint64, window time.Duration) (*RateLimiter, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewRateLimiter(threshold, window)
	l.now = clk.now
	return l, clk
}

// tickFor advances the fake clock and the limiter together, one second at a time.
func tickFor(l *RateLimiter, clk *fakeClock, d time.Duration) {
	for i := 0; i < int(d/time.Second); i++ {
		clk.advance(time.Second)
		l.tick()
	}
}

func ipN(i int) string { return fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255) }

func TestRateLimiterBlocksImmediately(t *testing.T) {
	l, _ := newTestLimiter(100, 10*time.Second)
	for i := 0; i < 101; i++ {
		l.observe("203.0.113.1")
	}
	for i := 0; i < 20; i++ {
		l.observe("203.0.113.2")
	}
	// No tick needed: the block lands on the request that crosses the line.
	if !l.Blocked("203.0.113.1") {
		t.Errorf("IP over the threshold was not blocked")
	}
	if l.Blocked("203.0.113.2") {
		t.Errorf("light IP wrongly blocked")
	}
}

// TestRateLimiterCatchesMoreThanTopKOffenders is the botnet case the old
// design failed: it could only block IPs holding one of 1,024 top-K slots.
func TestRateLimiterCatchesMoreThanTopKOffenders(t *testing.T) {
	const (
		abusers = 5000
		light   = 2000
	)
	l, _ := newTestLimiter(100, 10*time.Second)
	for round := 0; round < 150; round++ { // interleaved, like real traffic
		for i := 0; i < abusers; i++ {
			l.observe(ipN(i))
		}
		if round < 10 {
			for i := 0; i < light; i++ {
				l.observe(ipN(100000 + i))
			}
		}
	}
	missed := 0
	for i := 0; i < abusers; i++ {
		if !l.Blocked(ipN(i)) {
			missed++
		}
	}
	if missed != 0 {
		t.Errorf("%d of %d abusive IPs were not blocked", missed, abusers)
	}
	falsePos := 0
	for i := 0; i < light; i++ {
		if l.Blocked(ipN(100000 + i)) {
			falsePos++
		}
	}
	if rate := float64(falsePos) / light; rate > 0.01 {
		t.Errorf("false-positive rate %.2f%% (%d/%d light IPs blocked), want <= 1%%", rate*100, falsePos, light)
	}
	_, total := l.blockedList(20)
	t.Logf("blocked %d IPs; %d/%d light IPs falsely blocked", total, falsePos, light)
}

// TestRateLimiterSlidingWindowStopsBoundaryBurst: with a fixed window, 100
// requests just before a reset plus 100 just after would pass a 100/10s limit.
// The sliding estimate still counts the earlier burst.
func TestRateLimiterSlidingWindowStopsBoundaryBurst(t *testing.T) {
	l, clk := newTestLimiter(100, 10*time.Second)
	tickFor(l, clk, 9*time.Second)
	for i := 0; i < 90; i++ {
		l.observe("198.51.100.5")
	}
	tickFor(l, clk, time.Second) // window rotates here
	for i := 0; i < 20; i++ {
		l.observe("198.51.100.5")
	}
	if !l.Blocked("198.51.100.5") {
		t.Errorf("burst straddling the window boundary was not blocked")
	}
}

func TestRateLimiterBlockExpiresWhenClientStops(t *testing.T) {
	l, clk := newTestLimiter(50, 5*time.Second)
	for i := 0; i < 200; i++ {
		l.observe("203.0.113.9")
	}
	if !l.Blocked("203.0.113.9") {
		t.Fatal("expected block")
	}
	tickFor(l, clk, 4*time.Second)
	if !l.Blocked("203.0.113.9") {
		t.Errorf("block lifted before the window passed")
	}
	tickFor(l, clk, 2*time.Second)
	if l.Blocked("203.0.113.9") {
		t.Errorf("block still active after the client went quiet for a full window")
	}
	if _, total := l.blockedList(10); total != 0 {
		t.Errorf("expired entry not cleaned up: %d blocked", total)
	}
}

func TestRateLimiterBlocklistIsCapped(t *testing.T) {
	l, _ := newTestLimiter(1, 10*time.Second)
	l.maxBlocked = 100
	for i := 0; i < 500; i++ {
		l.observe(ipN(i))
		l.observe(ipN(i))
	}
	if _, total := l.blockedList(1); total != 100 {
		t.Errorf("blocked %d IPs, want the cap of 100", total)
	}
	if l.Overflow() == 0 {
		t.Errorf("overflow should count offenders beyond the cap")
	}
}

func TestRateLimiterMemoryIsBounded(t *testing.T) {
	l, _ := newTestLimiter(10, 5*time.Second)
	before := l.SizeBytes()
	for i := 0; i < 500000; i++ {
		l.observe(ipN(i))
	}
	if l.SizeBytes() != before {
		t.Errorf("sketch memory grew with distinct IPs: before=%d after=%d", before, l.SizeBytes())
	}
}

func BenchmarkRateLimiterObserve(b *testing.B) {
	l := NewRateLimiter(1000, 10*time.Second)
	ips := make([]string, 4096)
	for i := range ips {
		ips[i] = ipN(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.observe(ips[i%len(ips)])
	}
}

func BenchmarkRateLimiterBlocked(b *testing.B) {
	l := NewRateLimiter(1000, 10*time.Second)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = l.Blocked("203.0.113.7")
		}
	})
}
