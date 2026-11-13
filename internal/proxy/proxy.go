// Package proxy implements Vantage's data plane: a reverse proxy that forwards
// each request to a backend and emits one analytics Event per request. The
// emit is non-blocking, so measurement never adds latency to real traffic.
package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

// Observer receives one Event per proxied request. *analytics.Engine satisfies
// it; the indirection keeps the proxy independent of the engine internals and
// trivially testable with a fake.
type Observer interface {
	Observe(analytics.Event)
}

// Limiter decides whether a client IP should be rejected before its request is
// proxied. *analytics.Engine satisfies it.
type Limiter interface {
	Blocked(ip string) bool
}

// Proxy forwards traffic to a single backend and reports each request to an
// Observer.
type Proxy struct {
	rp       *httputil.ReverseProxy
	obs      Observer
	lim      Limiter // nil disables rate limiting (the default)
	ipHeader string  // "" = TCP peer address; otherwise the header a trusted edge sets
	internal map[string]http.Handler
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithLimiter rejects requests from flagged IPs with 429 before they reach the
// backend. Without it the proxy forwards every request.
func WithLimiter(l Limiter) Option {
	return func(p *Proxy) { p.lim = l }
}

// WithIPHeader reads the client IP from a header set by a trusted edge in
// front of Vantage (for example "Fly-Client-IP" on Fly.io), instead of the TCP
// peer address. "X-Forwarded-For" takes its first hop.
//
// Only use this behind an edge that overwrites the header: a client talking to
// Vantage directly can put anything in it, which would let one client pose as
// thousands (dodging the rate limiter and inflating visitor counts).
func WithIPHeader(header string) Option {
	return func(p *Proxy) { p.ipHeader = http.CanonicalHeaderKey(strings.TrimSpace(header)) }
}

// WithInternal serves path on the data plane with h instead of proxying it.
// Internal requests are not reported to the Observer, so the site owner
// checking their own stats doesn't show up in them.
func WithInternal(path string, h http.Handler) Option {
	return func(p *Proxy) {
		if p.internal == nil {
			p.internal = map[string]http.Handler{}
		}
		p.internal[path] = h
	}
}

// New builds a reverse proxy for target that reports to obs. It rewrites the
// outbound Host header to the target's host so the origin serves the right
// virtual host (a platform like Vercel routes by Host, so forwarding the
// client's "localhost:8080" would miss), and sets the X-Forwarded-* headers.
func New(target *url.URL, obs Observer, opts ...Option) *Proxy {
	// Go's default transport keeps only 2 idle connections per host, so under
	// concurrent load a proxy opens (and discards) a fresh origin connection
	// for nearly every request. That adds a TCP+TLS handshake to each request
	// and piles up TIME_WAIT sockets until the OS runs out of ports. A proxy
	// talks to one origin, so keep a real pool for it.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 512
	transport.MaxIdleConnsPerHost = 256
	transport.IdleConnTimeout = 90 * time.Second

	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p := &Proxy{rp: rp, obs: obs}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h, ok := p.internal[r.URL.Path]; ok {
		h.ServeHTTP(w, r)
		return
	}
	start := time.Now()
	ev := analytics.Event{
		IP:        p.clientIP(r),
		Method:    r.Method,
		Host:      r.Host,
		Path:      r.URL.Path,
		UserAgent: r.UserAgent(),
		Referer:   r.Referer(),
		Prefetch:  isPrefetch(r),
	}
	if p.lim != nil && p.lim.Blocked(ev.IP) {
		w.Header().Set("Retry-After", "10")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		ev.Status = http.StatusTooManyRequests
		ev.Latency = time.Since(start)
		ev.Blocked = true
		p.obs.Observe(ev)
		return
	}
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	p.rp.ServeHTTP(rec, r)
	ev.Status = rec.status
	ev.Latency = time.Since(start)
	ev.Bytes = rec.bytes
	p.obs.Observe(ev)
}

// isPrefetch spots speculative requests browsers and Next.js make for pages
// the visitor might open next; counting them would inflate page views.
func isPrefetch(r *http.Request) bool {
	if r.Header.Get("Next-Router-Prefetch") != "" {
		return true
	}
	for _, h := range []string{"Sec-Purpose", "Purpose", "X-Moz", "X-Purpose"} {
		if strings.Contains(strings.ToLower(r.Header.Get(h)), "prefetch") {
			return true
		}
	}
	return false
}

// recorder wraps the ResponseWriter to capture the status code and byte count
// the backend produced. Unwrap lets http.ResponseController reach the
// underlying writer for flushing and hijacking.
type recorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.written = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// clientIP returns the client's address: the configured trusted header if set
// and present, otherwise the TCP peer address (which a client cannot forge).
func (p *Proxy) clientIP(r *http.Request) string {
	switch p.ipHeader {
	case "":
	case "X-Forwarded-For":
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	default:
		if ip := strings.TrimSpace(r.Header.Get(p.ipHeader)); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
