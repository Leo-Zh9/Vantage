package analytics

import (
	"sync"
	"time"
)

const (
	// DefaultMaxBlocked caps how many IPs can be blocked at once. Each entry costs
	// roughly 100 bytes, so the cap bounds the blocklist near 6.5 MB even under a
	// large botnet; offenders beyond it are counted in BlockOverflow.
	DefaultMaxBlocked = 65536

	// limiterDepth × limiterWidth sizes each of the limiter's two window
	// sketches. A wide, shallow grid keeps false positives low when thousands of
	// heavy IPs share it: with 5,000 abusive IPs in one window, a light client
	// collides with an abuser in every row well under 1% of the time.
	limiterDepth = 4
	limiterWidth = 16384
)

// RateLimiter flags client IPs whose request rate over a sliding window exceeds
// a threshold.
//
// Detection happens on every request: the Count-Min estimate for the IP is
// checked as soon as it is incremented, so any number of IPs can be caught (the
// previous design could only flag IPs that held one of 1,024 top-K slots).
//
// The window slides: it blends the current and previous fixed windows,
// weighting the previous one by how much of it still overlaps, so a client
// cannot double its allowance by bursting either side of a window boundary.
//
// Blocked IPs live in a sync.Map with an expiry. The engine's single goroutine
// is the only writer; proxy goroutines read it lock-free on every request, and
// a new block takes effect on the very next request instead of the next tick.
type RateLimiter struct {
	cur, prev  *CountMinSketch
	threshold  uint64
	window     time.Duration
	windowSec  uint64
	elapsed    uint64 // whole seconds into the current window
	maxBlocked int
	now        func() time.Time

	blocked  sync.Map // ip string -> blockEntry
	nBlocked int      // writer-owned
	overflow uint64   // writer-owned: offenders not blocked because the list was full
}

type blockEntry struct {
	until int64  // unix nanos; the block lapses after this unless refreshed
	count uint64 // estimated requests in the window when last refreshed
}

// NewRateLimiter flags an IP once its estimated request count within the
// sliding window exceeds threshold.
func NewRateLimiter(threshold uint64, window time.Duration) *RateLimiter {
	if window < time.Second {
		window = time.Second
	}
	if threshold < 1 {
		threshold = 1
	}
	return &RateLimiter{
		cur:        NewCountMinSketchDims(limiterDepth, limiterWidth),
		prev:       NewCountMinSketchDims(limiterDepth, limiterWidth),
		threshold:  threshold,
		window:     window,
		windowSec:  uint64(window / time.Second),
		maxBlocked: DefaultMaxBlocked,
		now:        time.Now,
	}
}

// observe records one request from ip and blocks it if its sliding-window
// estimate exceeds the threshold. Blocked requests are observed too, so a
// client that keeps hammering while blocked stays blocked; once it stops, the
// window drains and the block expires.
func (l *RateLimiter) observe(ip string) {
	if ip == "" {
		return
	}
	est := l.estimateAdd(ip)
	if est <= l.threshold {
		return
	}
	now := l.now().UnixNano()
	until := now + int64(l.window)
	if v, ok := l.blocked.Load(ip); ok {
		// Refresh only when the block is past its halfway point, so a flood from
		// one IP doesn't rewrite the map on every request.
		if v.(blockEntry).until-now < int64(l.window)/2 {
			l.blocked.Store(ip, blockEntry{until: until, count: est})
		}
		return
	}
	if l.nBlocked >= l.maxBlocked {
		l.overflow++
		return
	}
	l.blocked.Store(ip, blockEntry{until: until, count: est})
	l.nBlocked++
}

// estimateAdd increments ip in the current window and returns its sliding
// estimate: current count plus the previous window's count weighted by the
// fraction of it still inside the sliding window.
func (l *RateLimiter) estimateAdd(ip string) uint64 {
	cur := l.cur.AddAndCount(ip)
	prev := l.prev.Count(ip)
	if prev == 0 {
		return cur
	}
	return cur + prev*(l.windowSec-l.elapsed)/l.windowSec
}

// Blocked reports whether ip is currently blocked. It is safe to call from any
// goroutine and never blocks.
func (l *RateLimiter) Blocked(ip string) bool {
	v, ok := l.blocked.Load(ip)
	return ok && v.(blockEntry).until > l.now().UnixNano()
}

// tick advances time by one second: it rotates the window when it has fully
// elapsed and drops expired blocks.
func (l *RateLimiter) tick() {
	l.elapsed++
	if l.elapsed >= l.windowSec {
		l.prev, l.cur = l.cur, l.prev
		l.cur.Reset()
		l.elapsed = 0
	}
	now := l.now().UnixNano()
	l.blocked.Range(func(k, v any) bool {
		if v.(blockEntry).until <= now {
			l.blocked.Delete(k)
			l.nBlocked--
		}
		return true
	})
}

// blockedList returns up to n currently blocked IPs with their windowed
// counts, highest first, plus the total number blocked.
func (l *RateLimiter) blockedList(n int) ([]KeyCount, int) {
	out := make([]KeyCount, 0, l.nBlocked)
	now := l.now().UnixNano()
	l.blocked.Range(func(k, v any) bool {
		if e := v.(blockEntry); e.until > now {
			out = append(out, KeyCount{Key: k.(string), Count: e.count})
		}
		return true
	})
	total := len(out)
	sortKeyCounts(out)
	if len(out) > n {
		out = out[:n]
	}
	return out, total
}

// Overflow is how many over-limit requests went unblocked because the
// blocklist was full.
func (l *RateLimiter) Overflow() uint64 { return l.overflow }

// SizeBytes is the fixed footprint of the two window sketches. The blocklist is
// bounded separately by maxBlocked.
func (l *RateLimiter) SizeBytes() int { return l.cur.SizeBytes() + l.prev.SizeBytes() }
