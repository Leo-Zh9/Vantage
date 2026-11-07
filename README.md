# Vantage

[![CI](https://github.com/Leo-Zh9/Vantage/actions/workflows/ci.yml/badge.svg)](https://github.com/Leo-Zh9/Vantage/actions/workflows/ci.yml)

A Go reverse proxy that computes **real-time traffic analytics and rate limiting in constant memory**.

Counting unique visitors or the busiest paths normally means storing every key,
so memory grows with traffic. Vantage answers the same questions from
probabilistic sketches whose size is fixed at startup (~610 KB total) no matter
how much traffic flows through. It uses only the standard library, with every
sketch implemented from scratch, and builds to one static binary.

```
            ┌─────────── data plane (:8080) ───────────┐
 client ───▶│  Vantage proxy  ──────────────▶ backend  │
            └──────────────┬───────────────────────────┘
                           │ Event{ip, path, status, latency, bytes}
                           │ non-blocking send; dropped if buffer full
                           ▼
                  ┌─────────────────────┐
                  │  analytics engine   │  single goroutine, no locks
                  │  • HyperLogLog      │  unique visitors
                  │  • Count-Min + top-K│  busiest paths / IPs, rate limiting
                  │  • histogram        │  p50 / p95 / p99 latency
                  └──────────┬──────────┘
                             │ immutable snapshot published 1×/s (atomic pointer)
            ┌──────── control plane (:9090) ────────────┐
            │  /  dashboard   /api/stats   /metrics     │
            └────────────────────────────────────────────┘
```

## Highlights

- **Fixed memory:** a 16 KB HyperLogLog counts unique IPs within 2% of truth from 1K to 1M distinct visitors.
- **No added hot-path latency:** the proxy hands each event to one analytics goroutine over a buffered channel. Under overload it drops (and counts) events instead of slowing real requests, and no locks are needed.
- **Abuse protection:** with `-ratelimit`, IPs over a request threshold in a rolling window get `429` before reaching the backend. Count-Min never undercounts, so heavy clients can't slip under the limit.
- **Horizontally mergeable:** HyperLogLog and Count-Min sketches merge across instances for a fleet-wide view without shipping logs.
- **Production hygiene:** Prometheus metrics, separate data and control ports, race-tested CI, golangci-lint, and a distroless Docker image.

## Quick start

```bash
python3 -m http.server 8000                                  # any backend
go run ./cmd/vantage -backend http://localhost:8000          # proxy :8080, dashboard :9090
go run ./tools/loadgen -url http://localhost:8080 -n 100000 -visitors 5000
open http://localhost:9090
```

`-backend` can also be a live HTTPS origin (e.g. `https://example.com`) to watch real-site traffic through the proxy.

| Flag | Default | Meaning |
|------|---------|---------|
| `-backend` | required | origin URL to proxy |
| `-listen` / `-admin` | `:8080` / `:9090` | data-plane / control-plane addresses |
| `-buffer` | `4096` | analytics event buffer size |
| `-ratelimit` / `-ratewindow` | `0` (off) / `10`s | max requests per IP per window |

## Performance

All analytics hot paths are **zero-allocation** (`go test -bench`, Apple Silicon):

| Operation | ns/op |
|-----------|-------|
| HyperLogLog `Add` | ~8 |
| Count-Min + top-K `Add` | ~107 |
| Histogram `Observe` | ~5 |
| Engine event intake | ~11 |

An end-to-end integration test drives 20K concurrent requests through
backend → proxy → engine → control plane and checks unique-visitor estimates
(within ~2%) and the busiest path, with sketch memory flat throughout.

```bash
make test    # unit + integration tests
make bench   # microbenchmarks
make docker  # static distroless image
```

See [DESIGN.md](DESIGN.md) for the data-structure tradeoffs and concurrency model.

## License

MIT
