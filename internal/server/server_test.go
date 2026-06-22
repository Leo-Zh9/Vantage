package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

type fakeStats struct{ s analytics.Snapshot }

func (f fakeStats) Snapshot() analytics.Snapshot { return f.s }

func sample() analytics.Snapshot {
	return analytics.Snapshot{
		TotalRequests:  4200,
		TotalBytes:     1 << 20,
		RequestsPerSec: 37,
		UniqueVisitors: 512,
		LatencyP50:     1.25,
		LatencyP95:     4.5,
		LatencyP99:     9.0,
		StatusClasses:  map[string]uint64{"2xx": 4000, "5xx": 200},
		TopPaths:       []analytics.KeyCount{{Key: "/api/health", Count: 3000}},
		MemoryBytes:    200000,
	}
}

func TestStatsJSON(t *testing.T) {
	h := NewAdmin(fakeStats{sample()})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var got analytics.Snapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.TotalRequests != 4200 || got.UniqueVisitors != 512 {
		t.Errorf("decoded snapshot mismatch: %+v", got)
	}
}

func TestMetricsExposition(t *testing.T) {
	h := NewAdmin(fakeStats{sample()})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()
	for _, want := range []string{
		"vantage_requests_total 4200",
		"vantage_unique_visitors 512",
		`vantage_request_latency_ms{quantile="0.99"} 9`,
		`vantage_responses_total{class="5xx"} 200`,
		`vantage_top_path_requests{path="/api/health"} 3000`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", want, body)
		}
	}
}

func TestHealthAndDashboard(t *testing.T) {
	h := NewAdmin(fakeStats{sample()})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if b, _ := io.ReadAll(rr.Body); string(b) != "ok" {
		t.Errorf("healthz = %q, want ok", b)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rr.Body.String(), "VANTAGE") {
		t.Errorf("dashboard did not render")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/nonexistent", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", rr.Code)
	}
}
