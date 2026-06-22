// Command loadgen drives synthetic traffic at a Vantage proxy so the dashboard
// has something to show. It simulates many distinct visitors (via
// X-Forwarded-For) and a skewed path mix — a few hot paths plus a long cold
// tail — which is what makes the unique-visitor and top-path estimators
// interesting to watch.
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

func main() {
	base := flag.String("url", "http://localhost:8080", "proxy base URL to send traffic to")
	total := flag.Int("n", 100000, "total requests to send")
	workers := flag.Int("c", 50, "concurrent workers")
	visitors := flag.Int("visitors", 5000, "number of distinct simulated client IPs")
	hot := flag.Int("hot", 8, "number of hot paths carrying most of the traffic")
	cold := flag.Int("cold", 200, "number of cold (long-tail) paths")
	flag.Parse()

	paths := make([]string, 0, *hot+*cold)
	for i := 0; i < *hot; i++ {
		paths = append(paths, fmt.Sprintf("/api/hot/%d", i))
	}
	for i := 0; i < *cold; i++ {
		paths = append(paths, fmt.Sprintf("/static/asset-%d", i))
	}

	var sent, failed atomic.Int64
	var wg sync.WaitGroup
	jobs := make(chan struct{})
	start := time.Now()

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			client := &http.Client{Timeout: 10 * time.Second}
			for range jobs {
				path := paths[*hot+r.Intn(*cold)]
				if r.Float64() < 0.7 { // 70% of hits land on the hot paths
					path = paths[r.Intn(*hot)]
				}
				req, _ := http.NewRequest(http.MethodGet, *base+path, nil)
				id := r.Intn(*visitors)
				req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", (id>>16)&255, (id>>8)&255, id&255))
				resp, err := client.Do(req)
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
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
