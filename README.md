# Vantage

A reverse proxy that computes **real-time traffic analytics in constant memory**.

Most analytics pipelines answer "how many unique visitors?" or "what are the
busiest paths?" by storing every key and counting — memory grows with traffic.
Vantage sits in front of your service, observes every request, and answers the
same questions from **probabilistic sketches** whose memory stays flat no matter
how much traffic flows through: tens of kilobytes for unbounded cardinality.

```
            ┌─────────── data plane (:8080) ───────────┐
 client ───▶│  Vantage proxy  ──────────────▶ backend  │
            └──────────────┬───────────────────────────┘
                           │ Event{ip, path, status, latency, bytes}
                           │ (non-blocking send; dropped if buffer full)
                           ▼
                  ┌─────────────────────┐
                  │  analytics engine   │  single goroutine, no locks
                  │  • HyperLogLog      │  unique visitors   (~16 KB)
                  │  • Count-Min Sketch │  top paths / IPs
                  │  • histogram        │  p50 / p95 / p99
                  └──────────┬──────────┘
                             │ snapshot published once per second (atomic)
            ┌──────── control plane (:9090) ────────────┐
            │  /            live dashboard               │
            │  /api/stats   JSON snapshot                │
            │  /metrics     Prometheus exposition        │
            │  /healthz     liveness                     │
            └────────────────────────────────────────────┘
```

The data plane and control plane listen on **separate ports**, so dashboard and
metrics traffic never competes with proxied requests.

## Why it's interesting

- **Constant memory.** Unique-visitor counting uses a 16 KB HyperLogLog sketch
  whether it sees a thousand IPs or a billion. Total sketch footprint is fixed
  at startup (~610 KB by default) and never grows with traffic.
- **Zero added latency on the hot path.** The proxy emits one event per request
  over a buffered channel and moves on. A single analytics goroutine owns all
  the sketches, so there are no locks; under overload the proxy *drops* events
  (and counts the drops) rather than slowing real traffic.
- **No dependencies.** Pure Go standard library. The sketches are implemented
  from scratch. `go build` produces one static binary.

## Quick start

```bash
# 1. start any backend to put traffic in front of
python3 -m http.server 8000

# 2. run the proxy (forwards :8080 -> :8000, dashboard on :9090)
go run ./cmd/vantage -backend http://localhost:8000

# 3. generate some traffic (5000 distinct visitors, skewed path mix)
go run ./tools/loadgen -url http://localhost:8080 -n 100000 -visitors 5000

# 4. watch it live
open http://localhost:9090           # dashboard
curl localhost:9090/api/stats        # JSON
curl localhost:9090/metrics          # Prometheus
```

## Put it in front of a real site

`-backend` can be any origin, including a live HTTPS site. Vantage rewrites the
outbound `Host` header to the origin, so the backend serves the correct site:

```bash
go run ./cmd/vantage -backend https://example.com
# now browse http://localhost:8080 — you're viewing the real site *through*
# Vantage, and every request shows up on the dashboard at http://localhost:9090
```

This is a safe way to test against a real origin without touching its DNS or
real visitors. Routing real traffic through Vantage is a later step: deploy it
publicly (see [Deploy](#deploy)) and point your domain's DNS at it.

## Rate limiting / abuse detection

The same heavy-hitter machinery that ranks busy IPs can shed them. With
`-ratelimit` set, Vantage flags any client IP whose request count over the
rolling window exceeds the threshold and returns `429` before the request
reaches the backend:

```bash
go run ./cmd/vantage -backend http://localhost:8000 -ratelimit 1000 -ratewindow 10
# any IP over 1000 requests in a 10s window is throttled until it cools off
```

It tracks only the heaviest IPs, so its memory is fixed no matter how many
distinct clients appear. Because the backing Count-Min Sketch never undercounts,
an abuser can never slip under the threshold — at worst a borderline-heavy client
is flagged slightly early, which is the safe direction. The analytics goroutine
owns the limiter and publishes the blocked set once per second; the proxy reads
it without a lock, so enforcement adds nothing to the hot path. Throttling is
exported as `vantage_throttled_total` and `vantage_blocked_ips`.

## Scale horizontally

Every estimator is **mergeable**: HyperLogLog folds by register-wise maximum and
the Count-Min Sketch by cell-wise sum. Run an instance per node and merge their
sketches to get one global view of unique visitors and top talkers across the
fleet — without ever shipping per-request logs.

## Endpoints

| Plane    | Path          | Description                                  |
|----------|---------------|----------------------------------------------|
| data     | `*`           | everything is proxied to `-backend`          |
| control  | `/`           | live HTML dashboard (auto-refreshes)         |
| control  | `/api/stats`  | current snapshot as JSON                     |
| control  | `/metrics`    | Prometheus text exposition                   |
| control  | `/healthz`    | returns `ok`                                 |

## Flags

| Flag        | Default   | Meaning                                            |
|-------------|-----------|----------------------------------------------------|
| `-backend`  | (required)| origin URL to proxy, e.g. `http://localhost:8000`  |
| `-listen`   | `:8080`   | data-plane address (client traffic)                |
| `-admin`    | `:9090`   | control-plane address (dashboard/stats/metrics)    |
| `-buffer`   | `4096`    | analytics event buffer; events drop when full      |
| `-ratelimit`| `0`       | max requests per client IP per window; `0` disables |
| `-ratewindow`| `10`     | rate-limit rolling window, in seconds              |

## How the estimators work

| Question                | Structure          | Footprint   | Measured accuracy            |
|-------------------------|--------------------|-------------|------------------------------|
| How many unique IPs?    | HyperLogLog (p=14) | 16 KB fixed | ≤2% across 1k–1M distinct    |
| Which paths/IPs busiest?| Count-Min Sketch + top-K | fixed | recovers true top-10         |
| What's the latency tail?| bucketed histogram | fixed       | p95/p99 within ~1–2%         |

See [DESIGN.md](DESIGN.md) for the full rationale and the concurrency model.

## Performance

Microbenchmarks on the analytics hot paths (Apple Silicon, `go test -bench`),
all **zero-allocation**:

| Operation                       | ns/op | allocs |
|---------------------------------|-------|--------|
| HyperLogLog `Add`               | ~8    | 0      |
| Count-Min + top-K `Add`         | ~107  | 0      |
| Histogram `Observe`             | ~5    | 0      |
| Engine event intake (`Observe`) | ~11   | 0      |

An end-to-end integration test wires backend → proxy → engine → control plane
and drives 20k concurrent requests: it sustains thousands of req/s on a laptop,
estimates unique visitors within ~2% of truth, and identifies the correct
busiest path — with sketch memory flat throughout.

```bash
make bench    # microbenchmarks
make test     # unit + integration tests (add -race in CI)
```

## Develop

```bash
make build    # -> bin/vantage
make test
make vet
make fmt
make docker   # builds a static image on distroless
```

## Deploy

Vantage is a single static binary, so hosting is cheap:

- **Docker:** `docker build -t vantage . && docker run -p 8080:8080 -p 9090:9090 vantage -backend https://your-origin`
- **Any VM / free tier:** copy the binary and run it under systemd; it needs no
  runtime, no database, and a few hundred KB of RAM for the sketches.

## License

MIT — see [LICENSE](LICENSE).
