// Package integration exercises the full request path end to end:
// client -> Vantage proxy -> backend, with the analytics engine and control
// plane wired exactly as cmd/vantage wires them.
package integration

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leo-Zh9/vantage/internal/analytics"
	"github.com/Leo-Zh9/vantage/internal/proxy"
	"github.com/Leo-Zh9/vantage/internal/server"
)

func wire(t testing.TB) (front, admin *httptest.Server, eng *analytics.Engine, stop func()) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	target, _ := url.Parse(backend.URL)
	eng = analytics.NewEngine(1 << 16)
	go eng.Run()
	front = httptest.NewServer(proxy.New(target, eng))
	admin = httptest.NewServer(server.NewAdmin(eng))
	stop = func() {
		front.Close()
		admin.Close()
		backend.Close()
		eng.Stop()
	}
	return
}

func TestEndToEnd(t *testing.T) {
	front, admin, eng, stop := wire(t)
	defer stop()

	const (
		n        = 20000
		workers  = 50
		visitors = 2000
		hot      = 5
	)
	client := front.Client()
	client.Transport.(*http.Transport).MaxIdleConnsPerHost = workers

	var done atomic.Int64
	var wg sync.WaitGroup
	jobs := make(chan int, n)
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)

	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for range jobs {
				path := fmt.Sprintf("/cold/%d", r.Intn(5000))
				if r.Float64() < 0.7 {
					path = fmt.Sprintf("/hot/%d", r.Intn(hot))
				}
				req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
				req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.%d.%d", r.Intn(visitors)/256, r.Intn(visitors)%256))
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done.Add(1)
			}
		}(int64(w) + 1)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if done.Load() != n {
		t.Fatalf("only %d/%d requests completed", done.Load(), n)
	}
	t.Logf("end-to-end: %d requests in %s = %.0f req/s through the proxy",
		n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())

	// Let the buffer drain and a publish tick land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && eng.Snapshot().TotalRequests < n {
		time.Sleep(10 * time.Millisecond)
	}

	s := eng.Snapshot()
	if s.TotalRequests != n {
		t.Fatalf("snapshot total = %d, want %d (dropped %d)", s.TotalRequests, n, s.Dropped)
	}
	if s.StatusClasses["2xx"] != n {
		t.Errorf("2xx = %d, want %d", s.StatusClasses["2xx"], n)
	}
	if relErr := math.Abs(float64(s.UniqueVisitors)-visitors) / visitors; relErr > 0.03 {
		t.Errorf("unique visitors = %d, want ~%d (relErr %.3f)", s.UniqueVisitors, visitors, relErr)
	}
	if len(s.TopPaths) == 0 || s.TopPaths[0].Key[:4] != "/hot" {
		t.Errorf("top path = %+v, want a /hot/* path", s.TopPaths)
	}
	t.Logf("unique visitors estimate=%d (actual %d) · top path=%s · p99=%.2fms · sketch mem=%dKB",
		s.UniqueVisitors, visitors, s.TopPaths[0].Key, s.LatencyP99, s.MemoryBytes/1024)

	// Control plane reflects the same numbers.
	resp, err := http.Get(admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := fmt.Sprintf("vantage_requests_total %d", n); !strings.Contains(string(body), want) {
		t.Errorf("/metrics missing %q", want)
	}
}
