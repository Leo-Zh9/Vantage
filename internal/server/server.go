// Package server exposes Vantage's control plane: a live HTML dashboard, a JSON
// stats API, a Prometheus metrics endpoint, and a health check. It reads only
// published snapshots, so it never races the analytics writer.
package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Leo-Zh9/vantage/internal/analytics"
)

//go:embed index.html
var indexHTML []byte

// Snapshotter is the read side of the analytics engine.
type Snapshotter interface {
	Snapshot() analytics.Snapshot
}

// NewAdmin returns the control-plane handler.
func NewAdmin(s Snapshotter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", stats(s))
	mux.HandleFunc("/metrics", metrics(s))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", dashboard)
	return mux
}

func dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func stats(s Snapshotter) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(s.Snapshot())
	}
}

func metrics(s Snapshotter) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snap := s.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder

		gaugeOrCounter(&b, "vantage_requests_total", "Total requests proxied since start.", "counter", float64(snap.TotalRequests))
		gaugeOrCounter(&b, "vantage_bytes_total", "Total response bytes proxied.", "counter", float64(snap.TotalBytes))
		gaugeOrCounter(&b, "vantage_requests_per_second", "Requests in the last completed second.", "gauge", float64(snap.RequestsPerSec))
		gaugeOrCounter(&b, "vantage_unique_visitors", "Estimated distinct client IPs (HyperLogLog p=14).", "gauge", float64(snap.UniqueVisitors))
		gaugeOrCounter(&b, "vantage_events_dropped_total", "Analytics events dropped due to a full buffer.", "counter", float64(snap.Dropped))
		gaugeOrCounter(&b, "vantage_sketch_memory_bytes", "Fixed memory held by the analytics sketches.", "gauge", float64(snap.MemoryBytes))

		header(&b, "vantage_request_latency_ms", "Estimated request latency quantiles (bucketed histogram).", "summary")
		fmt.Fprintf(&b, "vantage_request_latency_ms{quantile=\"0.5\"} %g\n", snap.LatencyP50)
		fmt.Fprintf(&b, "vantage_request_latency_ms{quantile=\"0.95\"} %g\n", snap.LatencyP95)
		fmt.Fprintf(&b, "vantage_request_latency_ms{quantile=\"0.99\"} %g\n", snap.LatencyP99)

		header(&b, "vantage_responses_total", "Responses by HTTP status class.", "counter")
		for _, class := range sortedKeys(snap.StatusClasses) {
			fmt.Fprintf(&b, "vantage_responses_total{class=%q} %d\n", class, snap.StatusClasses[class])
		}

		header(&b, "vantage_top_path_requests", "Estimated request count for the busiest paths.", "gauge")
		for _, kc := range snap.TopPaths {
			fmt.Fprintf(&b, "vantage_top_path_requests{path=%q} %d\n", escapeLabel(kc.Key), kc.Count)
		}

		_, _ = w.Write([]byte(b.String()))
	}
}

func header(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func gaugeOrCounter(b *strings.Builder, name, help, typ string, v float64) {
	header(b, name, help, typ)
	if v == float64(int64(v)) {
		fmt.Fprintf(b, "%s %d\n", name, int64(v))
	} else {
		fmt.Fprintf(b, "%s %g\n", name, v)
	}
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
