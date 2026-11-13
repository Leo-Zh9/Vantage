package analytics

import (
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// saveEvery is how often (in one-second ticks) daily state is persisted.
const saveEvery = 60

// historyInSnapshot is how many finished days a Snapshot carries.
const historyInSnapshot = 30

// Event is one observed request, emitted by the proxy and consumed by the
// Engine. It is a value type so it crosses the channel by copy — the request
// goroutine and the analytics goroutine never share mutable memory.
type Event struct {
	IP        string
	Method    string
	Host      string // Host the client asked for (used to spot internal referrers)
	Path      string
	UserAgent string
	Referer   string
	Prefetch  bool // speculative browser/framework prefetch, not a real view
	Status    int
	Latency   time.Duration
	Bytes     int64
	Blocked   bool // request was rejected by the rate limiter (429), not proxied
}

// Snapshot is an immutable view of the analytics state, rebuilt once per second
// and published atomically. HTTP handlers read a Snapshot without touching the
// live sketches, so reads never race the single writer.
type Snapshot struct {
	UptimeSeconds  float64           `json:"uptime_seconds"`
	StartedUnix    int64             `json:"started_unix"`
	TotalRequests  uint64            `json:"total_requests"`
	TotalBytes     uint64            `json:"total_bytes"`
	RequestsPerSec uint64            `json:"requests_per_sec"`
	UniqueVisitors uint64            `json:"unique_visitors"`
	HumanVisitors  uint64            `json:"human_visitors"`
	PageViews      uint64            `json:"page_views"`
	BotRequests    uint64            `json:"bot_requests"`
	ScannerProbes  uint64            `json:"scanner_probes"`
	Dropped        uint64            `json:"dropped_events"`
	LatencyP50     float64           `json:"latency_p50_ms"`
	LatencyP95     float64           `json:"latency_p95_ms"`
	LatencyP99     float64           `json:"latency_p99_ms"`
	LatencyMax     float64           `json:"latency_max_ms"`
	StatusClasses  map[string]uint64 `json:"status_classes"`
	TopPaths       []KeyCount        `json:"top_paths"`
	TopIPs         []KeyCount        `json:"top_ips"`
	TopPages       []KeyCount        `json:"top_pages"`
	TopReferrers   []KeyCount        `json:"top_referrers"`
	TopProbes      []KeyCount        `json:"top_probes"`
	ThrottledTotal uint64            `json:"throttled_total"`
	BlockedIPs     []KeyCount        `json:"blocked_ips"` // the 20 heaviest
	BlockedCount   int               `json:"blocked_count"`
	BlockOverflow  uint64            `json:"block_overflow"`
	Today          DayRow            `json:"today"`
	History        []DayRow          `json:"history"` // finished days, newest first
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
	limiter *RateLimiter // nil unless EnableRateLimit was called; set before Run

	// The fields below are owned exclusively by the run loop.
	start      time.Time
	visitors   *HyperLogLog // every client IP
	humans     *HyperLogLog // client IPs classified as human
	paths      *TopK
	ips        *TopK
	pages      *TopK // human page views
	refs       *TopK // external referrer hosts of human page views
	probes     *TopK // paths requested by scanners
	latency    *Histogram
	status     map[int]uint64
	totalReq   uint64
	totalByt   uint64
	pageViews  uint64
	classCount [3]uint64
	throttled  uint64
	curSec     uint64
	rps        *ring
	day        *daily
	statePath  string
	ticks      int
}

// NewEngine creates an Engine whose intake channel buffers bufferSize events.
func NewEngine(bufferSize int) *Engine {
	if bufferSize < 1 {
		bufferSize = 1
	}
	now := time.Now()
	e := &Engine{
		events:   make(chan Event, bufferSize),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		start:    now,
		visitors: NewHyperLogLog(14),
		humans:   NewHyperLogLog(14),
		paths:    NewTopK(10, 0.001, 0.001),
		ips:      NewTopK(10, 0.001, 0.001),
		pages:    NewTopK(10, 0.002, 0.001),
		refs:     NewTopK(10, 0.002, 0.001),
		probes:   NewTopK(10, 0.002, 0.001),
		latency:  NewHistogram(),
		status:   make(map[int]uint64),
		rps:      newRing(60),
		day:      newDaily(time.UTC, now),
	}
	e.publish() // seed an empty snapshot so early reads are well-formed
	return e
}

// EnableRateLimit turns on per-IP rate limiting. It must be called before Run.
// An IP whose estimated request count over the sliding window exceeds threshold
// is blocked (429) until it slows down.
func (e *Engine) EnableRateLimit(threshold uint64, window time.Duration) {
	e.limiter = NewRateLimiter(threshold, window)
}

// ConfigureDaily sets the time zone that decides where one day ends and the
// next begins, and, if statePath is non-empty, restores and persists the
// per-day history there. It must be called before Run.
func (e *Engine) ConfigureDaily(loc *time.Location, statePath string) error {
	e.day = newDaily(loc, time.Now())
	e.statePath = statePath
	var err error
	if statePath != "" {
		err = e.day.load(statePath, time.Now())
	}
	e.publish()
	return err
}

// Blocked reports whether ip is currently rate limited. Safe to call from any
// goroutine; it never blocks.
func (e *Engine) Blocked(ip string) bool {
	return e.limiter != nil && e.limiter.Blocked(ip)
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
// the per-second counter, advances the rate-limit window, rolls the day over,
// and republishes the snapshot. It returns after Stop.
func (e *Engine) Run() {
	defer close(e.stopped)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-e.events:
			e.apply(ev)
		case now := <-ticker.C:
			e.rps.push(e.curSec)
			e.curSec = 0
			if e.limiter != nil {
				e.limiter.tick()
			}
			e.day.roll(now)
			e.ticks++
			if e.statePath != "" && e.ticks%saveEvery == 0 {
				e.save()
			}
			e.publish()
		case <-e.done:
			if e.statePath != "" {
				e.save()
			}
			return
		}
	}
}

func (e *Engine) save() {
	if err := e.day.save(e.statePath); err != nil {
		log.Printf("vantage: saving state to %s: %v", e.statePath, err)
	}
}

// Stop signals the run loop and waits for it to exit (saving state if
// persistence is on).
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
	e.day.today.Requests++

	class := classify(ev.UserAgent, ev.Path)
	e.classCount[class]++
	switch class {
	case Bot:
		e.day.today.BotRequests++
	case Scanner:
		e.day.today.ScannerProbes++
		e.probes.Add(ev.Path)
	}

	if ev.IP != "" {
		e.visitors.Add(ev.IP)
		e.ips.Add(ev.IP)
		if e.limiter != nil {
			e.limiter.observe(ev.IP)
		}
		if class == Human {
			e.humans.Add(ev.IP)
			e.day.visitors.Add(ev.IP)
		}
	}
	if ev.Path != "" {
		e.paths.Add(ev.Path)
	}
	if ev.Blocked {
		e.throttled++
		e.day.today.RateLimited++
	} else if class == Human && isPageView(ev) {
		e.pageViews++
		e.day.today.PageViews++
		e.pages.Add(ev.Path)
		if ref := referrerHost(ev.Referer, ev.Host); ref != "" {
			e.refs.Add(ref)
		}
	}
	e.latency.Observe(float64(ev.Latency) / float64(time.Millisecond))
	e.status[ev.Status/100]++
}

// isPageView reports whether a request is a page someone looked at: a
// successful GET for a document (not a script, image or font) that wasn't a
// speculative prefetch.
func isPageView(ev Event) bool {
	if ev.Method != "" && ev.Method != http.MethodGet {
		return false
	}
	return !ev.Prefetch && ev.Status < 400 && ev.Path != "" && !isAsset(ev.Path)
}

func (e *Engine) publish() {
	classes := make(map[string]uint64, len(e.status))
	for class, n := range e.status {
		classes[classLabel(class)] = n
	}
	memory := e.visitors.SizeBytes() + e.humans.SizeBytes() + e.day.visitors.SizeBytes() +
		e.paths.SizeBytes() + e.ips.SizeBytes() + e.pages.SizeBytes() + e.refs.SizeBytes() + e.probes.SizeBytes()
	var blockedIPs []KeyCount
	var blockedCount int
	var overflow uint64
	if e.limiter != nil {
		blockedIPs, blockedCount = e.limiter.blockedList(20)
		overflow = e.limiter.Overflow()
		memory += e.limiter.SizeBytes()
	}
	s := Snapshot{
		UptimeSeconds:  time.Since(e.start).Seconds(),
		StartedUnix:    e.start.Unix(),
		TotalRequests:  e.totalReq,
		TotalBytes:     e.totalByt,
		RequestsPerSec: e.rps.last(),
		UniqueVisitors: uint64(e.visitors.Estimate() + 0.5),
		HumanVisitors:  uint64(e.humans.Estimate() + 0.5),
		PageViews:      e.pageViews,
		BotRequests:    e.classCount[Bot],
		ScannerProbes:  e.classCount[Scanner],
		Dropped:        e.dropped.Load(),
		LatencyP50:     e.latency.Quantile(0.50),
		LatencyP95:     e.latency.Quantile(0.95),
		LatencyP99:     e.latency.Quantile(0.99),
		LatencyMax:     e.latency.Max(),
		StatusClasses:  classes,
		TopPaths:       e.paths.Top(),
		TopIPs:         e.ips.Top(),
		TopPages:       e.pages.Top(),
		TopReferrers:   e.refs.Top(),
		TopProbes:      e.probes.Top(),
		ThrottledTotal: e.throttled,
		BlockedIPs:     blockedIPs,
		BlockedCount:   blockedCount,
		BlockOverflow:  overflow,
		Today:          e.day.snapshotToday(),
		History:        e.day.recentHistory(historyInSnapshot),
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
