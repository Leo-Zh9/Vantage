// Command vantage is a reverse proxy that computes real-time traffic analytics
// (unique visitors, page views, bots, top paths, latency percentiles) in
// constant memory, rate limits abusive clients, and serves the results on a
// separate control-plane port and as a Markdown report.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; embed it for -tz

	"github.com/Leo-Zh9/vantage/internal/analytics"
	"github.com/Leo-Zh9/vantage/internal/proxy"
	"github.com/Leo-Zh9/vantage/internal/server"
	"github.com/Leo-Zh9/vantage/internal/version"
)

// reportPasswordEnv holds the password for the public report path. It is read
// from the environment (a Fly secret in production) so it never appears in a
// config file or the process list.
const reportPasswordEnv = "VANTAGE_REPORT_PASSWORD"

func main() {
	listen := flag.String("listen", ":8080", "data-plane address where the proxy accepts client traffic")
	admin := flag.String("admin", ":9090", "control-plane address for the dashboard, /api/stats, /metrics and /report.md")
	backend := flag.String("backend", "", "origin URL to proxy to, e.g. http://localhost:8000 (required)")
	buffer := flag.Int("buffer", 4096, "analytics event buffer size; events are dropped when full")
	rateLimit := flag.Int("ratelimit", 0, "max requests per client IP within -ratewindow before returning 429; 0 disables")
	rateWindow := flag.Int("ratewindow", 10, "rate-limit sliding window in seconds")
	ipHeader := flag.String("ip-header", "", "read the client IP from this header set by a trusted edge (e.g. Fly-Client-IP, X-Forwarded-For); empty uses the TCP peer address")
	site := flag.String("site", "", "site name shown in the report; defaults to the backend host")
	tz := flag.String("tz", "UTC", "IANA time zone that decides where each day starts, e.g. America/Toronto")
	state := flag.String("state", "", "file to persist per-day history across restarts (e.g. /data/state.json); empty keeps it in memory")
	reportFile := flag.String("report-file", "", "write the Markdown report to this file every -report-every")
	reportEvery := flag.Duration("report-every", 30*time.Second, "how often to rewrite -report-file")
	reportPath := flag.String("report-path", "", "also serve the report on the data plane at this path, behind Basic auth using $"+reportPasswordEnv)
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("vantage", version.String())
		return
	}
	if *backend == "" {
		log.Fatal("missing required -backend URL (e.g. -backend http://localhost:8000)")
	}
	target, err := url.Parse(*backend)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("invalid -backend %q: expected an absolute URL like http://host:port", *backend)
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatalf("invalid -tz %q: %v", *tz, err)
	}
	if *site == "" {
		*site = target.Host
	}
	reportOpts := server.ReportOptions{Site: *site, Location: loc}

	engine := analytics.NewEngine(*buffer)
	if err := engine.ConfigureDaily(loc, *state); err != nil {
		log.Printf("warning: could not restore state from %s (starting fresh): %v", *state, err)
	}
	proxyOpts := []proxy.Option{
		proxy.WithIPHeader(*ipHeader),
		// A health check on the data plane that never touches the origin, so the
		// platform's checks don't count as traffic or depend on the site.
		proxy.WithInternal("/_vantage/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})),
	}
	if *rateLimit > 0 {
		engine.EnableRateLimit(uint64(*rateLimit), time.Duration(*rateWindow)*time.Second)
		proxyOpts = append(proxyOpts, proxy.WithLimiter(engine))
		log.Printf("rate limit: %d req/IP per %ds sliding window", *rateLimit, *rateWindow)
	}
	if *ipHeader == "" {
		log.Printf("client IP: TCP peer address (set -ip-header when behind a trusted edge proxy)")
	} else {
		log.Printf("client IP: %s header (only safe behind an edge that overwrites it)", *ipHeader)
	}
	if *reportPath != "" {
		if pw := os.Getenv(reportPasswordEnv); pw != "" {
			proxyOpts = append(proxyOpts, proxy.WithInternal(*reportPath, server.NewReport(engine, reportOpts, pw)))
			log.Printf("report: https://<site>%s (Basic auth)", *reportPath)
		} else {
			log.Printf("warning: -report-path set but $%s is empty; report path disabled", reportPasswordEnv)
		}
	}
	go engine.Run()

	dataSrv := &http.Server{Addr: *listen, Handler: proxy.New(target, engine, proxyOpts...), ReadHeaderTimeout: 5 * time.Second}
	ctrlSrv := &http.Server{Addr: *admin, Handler: server.NewAdmin(engine, reportOpts), ReadHeaderTimeout: 5 * time.Second}

	go serve(dataSrv, fmt.Sprintf("data plane %s -> %s", *listen, target))
	go serve(ctrlSrv, fmt.Sprintf("control plane %s (dashboard /, /api/stats, /metrics, /report.md)", *admin))

	stopReport := make(chan struct{})
	reportDone := make(chan struct{})
	go func() {
		defer close(reportDone)
		if *reportFile == "" {
			return
		}
		writeReport(*reportFile, engine, reportOpts)
		t := time.NewTicker(*reportEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				writeReport(*reportFile, engine, reportOpts)
			case <-stopReport:
				return
			}
		}
	}()
	log.Printf("vantage %s started", version.String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = dataSrv.Shutdown(ctx)
	_ = ctrlSrv.Shutdown(ctx)
	engine.Stop() // persists daily state when -state is set
	close(stopReport)
	<-reportDone
	if *reportFile != "" {
		writeReport(*reportFile, engine, reportOpts)
	}
	log.Println("stopped")
}

func writeReport(path string, s server.Snapshotter, o server.ReportOptions) {
	if err := analytics.WriteFileAtomic(path, []byte(server.Markdown(s.Snapshot(), o))); err != nil {
		log.Printf("writing report %s: %v", path, err)
	}
}

func serve(s *http.Server, desc string) {
	log.Printf("listening: %s", desc)
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("%s: %v", desc, err)
	}
}
