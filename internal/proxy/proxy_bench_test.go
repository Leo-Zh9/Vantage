package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

func benchBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
}

// BenchmarkProxyBaseline measures a bare reverse proxy with no analytics. The
// delta between this and BenchmarkProxyInstrumented is the per-request cost of
// Vantage's measurement on the hot path.
func BenchmarkProxyBaseline(b *testing.B) {
	backend := benchBackend()
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	front := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer front.Close()
	driveProxy(b, front.URL)
}

// BenchmarkProxyInstrumented measures the same path with Vantage emitting one
// analytics event per request.
func BenchmarkProxyInstrumented(b *testing.B) {
	backend := benchBackend()
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	eng := analytics.NewEngine(1 << 16)
	go eng.Run()
	defer eng.Stop()
	front := httptest.NewServer(New(target, eng))
	defer front.Close()
	driveProxy(b, front.URL)
}

func driveProxy(b *testing.B, url string) {
	client := &http.Client{}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}
