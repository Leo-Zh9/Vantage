package analytics

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterFlagsHeavyHitters(t *testing.T) {
	l := NewRateLimiter(100, 2*time.Second)
	for i := 0; i < 300; i++ {
		l.observe("10.0.0.1") // abuser, well over the threshold
	}
	for i := 0; i < 20; i++ {
		l.observe("10.0.0.2") // light client, under the threshold
	}
	l.refresh()

	blocked := l.blockedSet()
	if _, ok := blocked["10.0.0.1"]; !ok {
		t.Errorf("heavy IP not flagged: %v", blocked)
	}
	if _, ok := blocked["10.0.0.2"]; ok {
		t.Errorf("light IP wrongly flagged: %v", blocked)
	}
}

func TestRateLimiterClearsAfterWindow(t *testing.T) {
	l := NewRateLimiter(50, 1*time.Second) // 1s window
	for i := 0; i < 200; i++ {
		l.observe("10.0.0.9")
	}
	l.tick() // refresh flags the IP, then the 1s window elapses and resets
	if _, ok := l.blockedSet()["10.0.0.9"]; !ok {
		t.Fatalf("expected IP flagged on first tick")
	}
	l.tick() // fresh window has no traffic, so the IP clears
	if _, ok := l.blockedSet()["10.0.0.9"]; ok {
		t.Errorf("expected IP cleared after the window reset")
	}
}

func TestRateLimiterMemoryIsBounded(t *testing.T) {
	l := NewRateLimiter(10, 5*time.Second)
	before := l.SizeBytes()
	for i := 0; i < 500000; i++ {
		l.observe(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if l.SizeBytes() != before {
		t.Errorf("limiter memory grew with distinct IPs: before=%d after=%d", before, l.SizeBytes())
	}
}
