# Vantage

[![CI](https://github.com/Leo-Zh9/Vantage/actions/workflows/ci.yml/badge.svg)](https://github.com/Leo-Zh9/Vantage/actions/workflows/ci.yml)

A Go reverse proxy that computes **real-time traffic analytics and rate limiting in constant memory**.

Counting unique visitors or the busiest paths normally means storing every key,
so memory grows with traffic. Vantage answers the same questions from
probabilistic sketches whose size is fixed at startup (~570 KB, or ~1.6 MB with
rate limiting on) no matter how much traffic flows through. It uses only the
standard library, with every sketch implemented from scratch, and builds to one
static binary.

```
            ┌─────────── data plane (:8080) ───────────┐
 client ───▶│  Vantage proxy  ──────────────▶ backend  │──▶ 429 if IP is blocked
            └──────────────┬───────────────────────────┘
                           │ Event{ip, path, ua, referer, status, latency}
                           │ non-blocking send; dropped if buffer full
                           ▼
                  ┌─────────────────────────┐
                  │  analytics engine       │  single goroutine, no locks
                  │  • HyperLogLog          │  unique visitors (all / human / per day)
                  │  • Count-Min + top-K    │  busiest paths, pages, referrers, probes
                  │  • sliding-window CMS   │  per-IP rate limiting → lock-free blocklist
                  │  • histogram            │  p50 / p95 / p99 latency
                  └──────────┬──────────────┘
                             │ immutable snapshot published 1×/s (atomic pointer)
            ┌──────── control plane (:9090) ────────────┐
            │  /  dashboard   /api/stats   /metrics   /report.md
            └────────────────────────────────────────────┘
```

## Highlights

- **Fixed memory:** a 16 KB HyperLogLog counts unique IPs within 2% of truth from 1K to 1M distinct visitors.
- **Rate limiting that scales with the attack:** every request's IP is checked against a sliding-window Count-Min estimate, so there is no cap on how many offenders can be caught. In a test with 5,000 abusive IPs, all 5,000 were blocked and 0 of 2,000 normal clients were wrongly blocked. The sliding window stops clients from doubling their allowance across a window boundary.
- **No added hot-path latency:** the proxy hands each event to one analytics goroutine over a buffered channel and never waits; the blocklist is a lock-free read (~1.7 ns).
- **Site analytics, not just traffic:** requests are classified as human, bot or vulnerability scanner. Page views exclude assets, prefetches and bots; referrers show where visitors came from. A **Markdown report** (per-day table, top pages, referrers, probed paths) is written to a file and served behind a password. It contains no IP addresses.
- **Survives restarts:** per-day history, including today's visitor sketch, is persisted to disk atomically.
- **Safe client IPs:** the client IP comes from the TCP connection unless you name a header set by a trusted edge (`-ip-header Fly-Client-IP`), so clients can't forge `X-Forwarded-For` to dodge the limiter.
- **Production hygiene:** Prometheus metrics, separate data and control ports, a pooled origin connection, race-tested CI, golangci-lint, and a distroless Docker image.

## Quick start

```bash
python3 -m http.server 8000                                       # any backend
go run ./cmd/vantage -backend http://localhost:8000 -ip-header X-Forwarded-For -ratelimit 100
go run ./tools/loadgen -url http://localhost:8080 -n 100000 -visitors 5000
open http://localhost:9090            # live dashboard
open http://localhost:9090/report.md  # Markdown report
```

`-ip-header X-Forwarded-For` lets the load generator simulate thousands of
visitors locally. Never use it on a server clients can reach directly.

| Flag | Default | Meaning |
|------|---------|---------|
| `-backend` | required | origin URL to proxy |
| `-listen` / `-admin` | `:8080` / `:9090` | data-plane / control-plane addresses |
| `-ip-header` | empty (TCP peer) | header a trusted edge sets with the client IP |
| `-ratelimit` / `-ratewindow` | `0` (off) / `10`s | max requests per IP per sliding window |
| `-site` / `-tz` | backend host / `UTC` | report title / time zone where each day starts |
| `-state` | empty | file to persist per-day history across restarts |
| `-report-file` / `-report-every` | empty / `30s` | write the Markdown report to a file |
| `-report-path` | empty | serve the report on the data plane behind Basic auth (`$VANTAGE_REPORT_PASSWORD`) |
| `-buffer` | `4096` | analytics event buffer size |

## Performance

Microbenchmarks (`make bench`, Windows x86-64, 12 threads). All hot paths are zero-allocation:

| Operation | ns/op |
|-----------|-------|
| HyperLogLog `Add` | ~15 |
| Engine event intake | ~17 |
| Top-K `Add`, k=10 | ~88 |
| Top-K `Add`, k=1024 | ~189 (was ~11,800 with a linear scan: **63× faster** with a min-heap) |
| Rate limiter `observe` | ~127 |
| Blocklist check (proxy side) | ~1.7 |

Pooling origin connections: 20,000 requests from 40 concurrent clients went
from 3,921 to 18,122 req/s, and left 31 sockets in `TIME_WAIT` instead of
16,570, which had exhausted the OS's ephemeral ports. (The old proxy used Go's
default of 2 idle connections per host, so it opened a new origin connection
for nearly every request.)

An end-to-end integration test drives 20K concurrent requests through
backend → proxy → engine → control plane and checks unique-visitor estimates
(within ~2%), the busiest path and the Markdown report, with sketch memory flat
throughout.

```bash
make test    # unit + integration tests
make bench   # microbenchmarks
make docker  # static distroless image
```

See [DESIGN.md](DESIGN.md) for the data-structure trade-offs and concurrency
model, and [DEPLOY.md](DEPLOY.md) for putting Vantage in front of a live site.

## License

MIT
