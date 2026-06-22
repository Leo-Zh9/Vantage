// Command vantage is a reverse proxy that computes real-time traffic analytics
// (unique visitors, top paths/IPs, latency percentiles) in constant memory and
// serves them on a separate control-plane port.
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

	"github.com/Leo-Zh9/vantage/internal/analytics"
	"github.com/Leo-Zh9/vantage/internal/proxy"
	"github.com/Leo-Zh9/vantage/internal/server"
	"github.com/Leo-Zh9/vantage/internal/version"
)

func main() {
	listen := flag.String("listen", ":8080", "data-plane address where the proxy accepts client traffic")
	admin := flag.String("admin", ":9090", "control-plane address for the dashboard, /api/stats, and /metrics")
	backend := flag.String("backend", "", "origin URL to proxy to, e.g. http://localhost:8000 (required)")
	buffer := flag.Int("buffer", 4096, "analytics event buffer size; events are dropped when full")
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

	engine := analytics.NewEngine(*buffer)
	go engine.Run()

	dataSrv := &http.Server{Addr: *listen, Handler: proxy.New(target, engine), ReadHeaderTimeout: 5 * time.Second}
	ctrlSrv := &http.Server{Addr: *admin, Handler: server.NewAdmin(engine), ReadHeaderTimeout: 5 * time.Second}

	go serve(dataSrv, fmt.Sprintf("data plane %s -> %s", *listen, target))
	go serve(ctrlSrv, fmt.Sprintf("control plane %s (dashboard / · stats /api/stats · metrics /metrics)", *admin))
	log.Printf("vantage %s started", version.String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = dataSrv.Shutdown(ctx)
	_ = ctrlSrv.Shutdown(ctx)
	engine.Stop()
	log.Println("stopped")
}

func serve(s *http.Server, desc string) {
	log.Printf("listening: %s", desc)
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("%s: %v", desc, err)
	}
}
