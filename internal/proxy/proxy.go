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
// proxied. *analytics.Engine satisfies it by reading its published blocked set.
type Limiter interface {
	Blocked(ip string) bool
}

// Proxy forwards traffic to a single backend and reports each request to an
// Observer.
type Proxy struct {
	rp  *httputil.ReverseProxy
	obs Observer
	lim Limiter // nil disables rate limiting (the default)
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithLimiter rejects requests from flagged IPs with 429 before they reach the
// backend. Without it the proxy forwards every request, as before.
func WithLimiter(l Limiter) Option {
	return func(p *Proxy) { p.lim = l }
}

// New builds a reverse proxy for target that reports to obs. It rewrites the
// outbound Host header to the target's host so the origin serves the right
// virtual host (a platform like Vercel routes by Host, so forwarding the
// client's "localhost:8080" would miss), and sets the X-Forwarded-* headers.
func New(target *url.URL, obs Observer, opts ...Option) *Proxy {
	rp := &httputil.ReverseProxy{
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
	start := time.Now()
	ip := clientIP(r)
	if p.lim != nil && p.lim.Blocked(ip) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		p.obs.Observe(analytics.Event{
			IP:      ip,
			Path:    r.URL.Path,
			Status:  http.StatusTooManyRequests,
			Latency: time.Since(start),
			Blocked: true,
		})
		return
	}
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	p.rp.ServeHTTP(rec, r)
	p.obs.Observe(analytics.Event{
		IP:      ip,
		Path:    r.URL.Path,
		Status:  rec.status,
		Latency: time.Since(start),
		Bytes:   rec.bytes,
	})
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

// clientIP prefers the first hop in X-Forwarded-For (set by upstream proxies /
// load balancers) and falls back to the TCP peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
