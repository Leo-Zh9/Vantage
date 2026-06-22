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

// Proxy forwards traffic to a single backend and reports each request to an
// Observer.
type Proxy struct {
	rp  *httputil.ReverseProxy
	obs Observer
}

// New builds a reverse proxy for target that reports to obs.
func New(target *url.URL, obs Observer) *Proxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusBadGateway)
	}
	return &Proxy{rp: rp, obs: obs}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	p.rp.ServeHTTP(rec, r)
	p.obs.Observe(analytics.Event{
		IP:      clientIP(r),
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
