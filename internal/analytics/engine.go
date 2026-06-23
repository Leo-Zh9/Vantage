package analytics

import (
	"sync/atomic"
	"time"
)

// Event is one observed request, emitted by the proxy and consumed by the
// Engine. It is a value type so it crosses the channel by copy — the request
// goroutine and the analytics goroutine never share mutable memory.
type Event struct {
	IP      string
	Path    string
	Status  int
	Latency time.Duration
	Bytes   int64
	Blocked bool // request was rejected by the rate limiter (429), not proxied
}

// Snapshot is an immutable view of the analytics state, rebuilt once per second
// and published atomically. HTTP handlers read a Snapshot without touching the
// live sketches, so reads never race the single writer.
type Snapshot struct {
	UptimeSeconds  float64           `json:"uptime_seconds"`
	TotalRequests  uint64            `json:"total_requests"`
	TotalBytes     uint64            `json:"total_bytes"`
	RequestsPerSec uint64            `json:"requests_per_sec"`
	UniqueVisitors uint64            `json:"unique_visitors"`
	Dropped        uint64            `json:"dropped_events"`
	LatencyP50     float64           `json:"latency_p50_ms"`
	LatencyP95     float64           `json:"latency_p95_ms"`
	LatencyP99     float64           `json:"latency_p99_ms"`
	LatencyMax     float64           `json:"latency_max_ms"`
	StatusClasses  map[string]uint64 `json:"status_classes"`
	TopPaths       []KeyCount        `json:"top_paths"`
	TopIPs         []KeyCount        `json:"top_ips"`
	ThrottledTotal uint64            `json:"throttled_total"`
	BlockedIPs     []KeyCount        `json:"blocked_ips"`
	RPSHistory     []uint64          `json:"rps_history"`
	MemoryBytes    int               `json:"sketch_memory_bytes"`
	GeneratedUnix  int64             `json:"generated_unix"`
}

// Engine aggregates traffic Events into constant-memory summaries. One goroutine
// owns every sketch, so they need no locks; producers communicate only through
// the buffered events channel. When the buffer is full the proxy drops events
// (counted in dropped) instead of blocking the request path — analytics is best
// effort and must never add latency to real traffic.
type Engine struct {
	events  chan Event
	done    chan struct{}
	stopped chan struct{}
	dropped atomic.Uint64
	snap    atomic.Pointer[Snapshot]
	blocked atomic.Pointer[map[string]struct{}] // rate-limited IPs; read lock-free by the proxy

	// The fields below are owned exclusively by the run loop.
	start     time.Time
	visitors  *HyperLogLog
	paths     *TopK
	ips       *TopK
	latency   *Histogram
	status    map[int]uint64
	totalReq  uint64
	totalByt  uint64
	throttled uint64
	curSec    uint64
	rps       *ring
	limiter   *RateLimiter // nil unless EnableRateLimit was called
}

// NewEngine creates an Engine whose intake channel buffers bufferSize events.
func NewEngine(bufferSize int) *Engine {
	if bufferSize < 1 {
		bufferSize = 1
	}
	e := &Engine{
		events:   make(chan Event, bufferSize),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		start:    time.Now(),
		visitors: NewHyperLogLog(14),
		paths:    NewTopK(10, 0.0005, 0.001),
		ips:      NewTopK(10, 0.0005, 0.001),
		latency:  NewHistogram(),
		status:   make(map[int]uint64),
		rps:      newRing(60),
	}
	e.publish() // seed an empty snapshot so early reads are well-formed
	return e
}

// EnableRateLimit turns on per-IP rate limiting. It must be called before Run.
// Requests from an IP whose count exceeds threshold within window receive a 429;
// the blocked set is recomputed each second and published for the proxy to read
// lock-free via Blocked.
func (e *Engine) EnableRateLimit(threshold uint64, window time.Duration) {
	e.limiter = NewRateLimiter(threshold, window)
	empty := map[string]struct{}{}
	e.blocked.Store(&empty)
}

// Blocked reports whether ip is currently over the rate-limit threshold. It
// reads an immutable published set, so the proxy calls it without locking.
func (e *Engine) Blocked(ip string) bool {
	set := e.blocked.Load()
	if set == nil {
		return false
	}
	_, ok := (*set)[ip]
	return ok
}

// Observe hands an event to the analytics goroutine without blocking. A full
// buffer means analytics is behind; the event is dropped and counted.
func (e *Engine) Observe(ev Event) {
	select {
	case e.events <- ev:
	default:
		e.dropped.Add(1)
	}
}

// Run is the single-writer loop. It drains events, and once per second rotates
// the per-second counter and republishes the snapshot. It returns after Stop.
func (e *Engine) Run() {
	defer close(e.stopped)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-e.events:
			e.apply(ev)
		case <-ticker.C:
			e.rps.push(e.curSec)
			e.curSec = 0
			if e.limiter != nil {
				e.limiter.tick()
				set := e.limiter.blockedSet()
				e.blocked.Store(&set)
			}
			e.publish()
		case <-e.done:
			return
		}
	}
}

// Stop signals the run loop and waits for it to exit.
func (e *Engine) Stop() {
	select {
	case <-e.done:
	default:
		close(e.done)
	}
	<-e.stopped
}

// Snapshot returns the most recently published view.
func (e *Engine) Snapshot() Snapshot {
	if s := e.snap.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

func (e *Engine) apply(ev Event) {
	e.totalReq++
	e.curSec++
	e.totalByt += uint64(ev.Bytes)
	if ev.IP != "" {
		e.visitors.Add(ev.IP)
		e.ips.Add(ev.IP)
		if e.limiter != nil {
			e.limiter.observe(ev.IP)
		}
	}
	if ev.Path != "" {
		e.paths.Add(ev.Path)
	}
	if ev.Blocked {
		e.throttled++
	}
	e.latency.Observe(float64(ev.Latency) / float64(time.Millisecond))
	e.status[ev.Status/100]++
}

func (e *Engine) publish() {
	classes := make(map[string]uint64, len(e.status))
	for class, n := range e.status {
		classes[classLabel(class)] = n
	}
	memory := e.visitors.SizeBytes() + e.paths.SizeBytes() + e.ips.SizeBytes()
	var blockedIPs []KeyCount
	if e.limiter != nil {
		blockedIPs = e.limiter.blockedList()
		memory += e.limiter.SizeBytes()
	}
	s := Snapshot{
		UptimeSeconds:  time.Since(e.start).Seconds(),
		TotalRequests:  e.totalReq,
		TotalBytes:     e.totalByt,
		RequestsPerSec: e.rps.last(),
		UniqueVisitors: uint64(e.visitors.Estimate() + 0.5),
		Dropped:        e.dropped.Load(),
		LatencyP50:     e.latency.Quantile(0.50),
		LatencyP95:     e.latency.Quantile(0.95),
		LatencyP99:     e.latency.Quantile(0.99),
		LatencyMax:     e.latency.Max(),
		StatusClasses:  classes,
		TopPaths:       e.paths.Top(),
		TopIPs:         e.ips.Top(),
		ThrottledTotal: e.throttled,
		BlockedIPs:     blockedIPs,
		RPSHistory:     e.rps.values(),
		MemoryBytes:    memory,
		GeneratedUnix:  time.Now().Unix(),
	}
	e.snap.Store(&s)
}

func classLabel(class int) string {
	switch class {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "other"
	}
}

// ring is a fixed-size circular buffer of per-second request counts, used to
// drive the dashboard's throughput sparkline in constant memory.
type ring struct {
	buf []uint64
	pos int
}

func newRing(n int) *ring { return &ring{buf: make([]uint64, n)} }

func (r *ring) push(v uint64) {
	r.buf[r.pos] = v
	r.pos = (r.pos + 1) % len(r.buf)
}

func (r *ring) last() uint64 {
	return r.buf[(r.pos-1+len(r.buf))%len(r.buf)]
}

// values returns the buffer oldest-first.
func (r *ring) values() []uint64 {
	out := make([]uint64, len(r.buf))
	for i := range r.buf {
		out[i] = r.buf[(r.pos+i)%len(r.buf)]
	}
	return out
}
