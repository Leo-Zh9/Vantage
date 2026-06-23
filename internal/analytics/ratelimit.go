package analytics

import (
	"sort"
	"time"
)

// trackedIPs bounds the rate limiter's memory: it watches only the heaviest
// client IPs (an abuser is a heavy hitter by definition), so the footprint is
// fixed no matter how many distinct IPs appear in the window.
const trackedIPs = 1024

// RateLimiter flags client IPs whose request count over a rolling window exceeds
// a threshold. It is built on a TopK heavy-hitter tracker, so memory stays flat
// regardless of how many distinct IPs are seen. The Engine's single writer owns
// it; the proxy only reads the published blocked set, so enforcement is lock-free.
//
// The backing Count-Min Sketch never undercounts, so the limiter never lets an
// abuser slip under the threshold — at worst it flags a borderline-heavy IP a
// little early, which is the safe direction for a limiter.
type RateLimiter struct {
	hot       *TopK
	threshold uint64
	windowSec int
	elapsed   int
	blocked   map[string]uint64
}

// NewRateLimiter flags an IP once its estimated request count within window
// exceeds threshold.
func NewRateLimiter(threshold uint64, window time.Duration) *RateLimiter {
	sec := int(window / time.Second)
	if sec < 1 {
		sec = 1
	}
	if threshold < 1 {
		threshold = 1
	}
	return &RateLimiter{
		hot:       NewTopK(trackedIPs, 0.001, 0.001),
		threshold: threshold,
		windowSec: sec,
		blocked:   make(map[string]uint64),
	}
}

// observe records one request from ip in the current window.
func (l *RateLimiter) observe(ip string) {
	if ip != "" {
		l.hot.Add(ip)
	}
}

// tick advances the window one second: it refreshes the blocked set from the
// window's heavy hitters, then clears the window once it has fully elapsed.
func (l *RateLimiter) tick() {
	l.refresh()
	l.elapsed++
	if l.elapsed >= l.windowSec {
		l.hot.Reset()
		l.elapsed = 0
	}
}

func (l *RateLimiter) refresh() {
	blocked := make(map[string]uint64)
	for _, kc := range l.hot.Top() {
		if kc.Count > l.threshold {
			blocked[kc.Key] = kc.Count
		}
	}
	l.blocked = blocked
}

// blockedSet returns the currently-blocked IPs as an immutable set for the proxy
// to read without locking.
func (l *RateLimiter) blockedSet() map[string]struct{} {
	set := make(map[string]struct{}, len(l.blocked))
	for ip := range l.blocked {
		set[ip] = struct{}{}
	}
	return set
}

// blockedList returns the blocked IPs with their windowed counts, highest first.
func (l *RateLimiter) blockedList() []KeyCount {
	out := make([]KeyCount, 0, len(l.blocked))
	for ip, n := range l.blocked {
		out = append(out, KeyCount{Key: ip, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// SizeBytes is the fixed footprint of the heavy-hitter tracker.
func (l *RateLimiter) SizeBytes() int { return l.hot.SizeBytes() }
