// Command loadgen drives synthetic traffic at a Vantage proxy so the dashboard
// and report have something to show. It simulates many distinct visitors (via
// X-Forwarded-For, so run Vantage with -ip-header X-Forwarded-For) and a
// realistic mix for a personal site: browsers opening a few hot pages and their
// assets, crawlers, vulnerability scanners, and visitors arriving from
// LinkedIn or GitHub.
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	pages     = []string{"/", "/projects", "/projects/vantage", "/about", "/resume.pdf", "/blog"}
	botUAs    = []string{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "LinkedInBot/1.0", "curl/8.7.1", "python-requests/2.32.3"}
	probes    = []string{"/wp-login.php", "/.env", "/.git/config", "/xmlrpc.php", "/phpmyadmin/", "/wp-admin/setup-config.php"}
	referrers = []string{"https://www.linkedin.com/", "https://github.com/", "https://www.google.com/", ""}
)

func main() {
	base := flag.String("url", "http://localhost:8080", "proxy base URL to send traffic to")
	total := flag.Int("n", 100000, "total requests to send")
	workers := flag.Int("c", 50, "concurrent workers")
	visitors := flag.Int("visitors", 5000, "number of distinct simulated client IPs")
	botShare := flag.Float64("bots", 0.2, "fraction of requests from crawlers and HTTP clients")
	scanShare := flag.Float64("scanners", 0.05, "fraction of requests probing for vulnerabilities")
	assets := flag.Int("assets", 200, "number of distinct static asset paths")
	flag.Parse()

	var sent, failed atomic.Int64
	var wg sync.WaitGroup
	jobs := make(chan struct{})
	start := time.Now()

	// One shared client whose pool fits every worker, so connections are reused
	// instead of churned (the default keeps just 2 idle connections per host).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = *workers
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for range jobs {
				path, ua, ref := pages[skewed(r, len(pages))], browserUA, ""
				switch x := r.Float64(); {
				case x < *scanShare:
					path, ua = probes[r.Intn(len(probes))], botUAs[2+r.Intn(2)]
				case x < *scanShare+*botShare:
					ua = botUAs[r.Intn(len(botUAs))]
				case r.Float64() < 0.6: // browsers fetch more assets than pages
					path = fmt.Sprintf("/_next/static/chunk-%d.js", r.Intn(*assets))
				default:
					ref = referrers[r.Intn(len(referrers))]
				}
				req, _ := http.NewRequest(http.MethodGet, *base+path, nil)
				id := r.Intn(*visitors)
				req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", (id>>16)&255, (id>>8)&255, id&255))
				req.Header.Set("User-Agent", ua)
				if ref != "" {
					req.Header.Set("Referer", ref)
				}
				resp, err := client.Do(req)
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				sent.Add(1)
			}
		}(int64(w) + 1)
	}

	for i := 0; i < *total; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()

	elapsed := time.Since(start)
	fmt.Printf("sent=%d failed=%d elapsed=%s throughput=%.0f req/s\n",
		sent.Load(), failed.Load(), elapsed.Round(time.Millisecond),
		float64(sent.Load())/elapsed.Seconds())
}

// skewed picks an index in [0, n) with a heavy bias toward the front, like
// real page popularity (the home page gets most views).
func skewed(r *rand.Rand, n int) int {
	for i := 0; i < n-1; i++ {
		if r.Float64() < 0.5 {
			return i
		}
	}
	return n - 1
}
