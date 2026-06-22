# Design notes

This document explains *why* Vantage is built the way it is. It doubles as a
reference for the tradeoffs behind each data structure.

## The problem

A reverse proxy sees every request. Useful operational questions about that
stream — *how many distinct visitors, which paths are hottest, what does the
latency tail look like* — are easy to answer if you store everything, and hard
to answer if you can't. Storing everything means memory grows with traffic:
a `map[string]int` of client IPs on a busy edge node is unbounded.

Vantage answers all three questions in **memory fixed at startup**, by trading
exact answers for tight probabilistic estimates. This is the same class of
technique a CDN or large-scale edge network uses, because at "Internet scale"
exact per-key state simply doesn't fit.

## Concurrency model: one writer, no locks

The request path and the analytics path are decoupled by a single buffered
channel:

```
proxy goroutine(s) ──Event──▶ [buffered channel] ──▶ analytics goroutine
```

- **The proxy never blocks on analytics.** `Engine.Observe` does a non-blocking
  channel send. If the buffer is full (analytics fell behind), the event is
  dropped and a counter increments. Measurement must never add latency or
  backpressure to real traffic — dropping a sample is always better than
  slowing a request.
- **One goroutine owns every sketch.** Because only the analytics goroutine
  touches the HyperLogLog / Count-Min / histogram state, there are *no mutexes*
  on the update path. This is Go's "share memory by communicating" model: the
  `Event` value crosses the channel by copy, so there is no shared mutable
  state to race on.
- **Readers never touch live state.** Once per second the engine builds an
  immutable `Snapshot` and publishes it with `atomic.Pointer`. The HTTP
  handlers (`/api/stats`, `/metrics`, dashboard) load the latest snapshot
  pointer — wait-free, and verified race-clean with `go test -race`.

`Event` is a plain value type for exactly this reason: passing it by value over
the channel guarantees the producer and consumer never alias the same memory.

## HyperLogLog — unique visitors

Counts distinct client IPs. With precision `p = 14` it keeps `m = 2^14 = 16384`
one-byte registers (16 KB) and has a standard error of `1.04/√m ≈ 0.81%`.

How a single observation updates it:
1. Hash the key to 64 bits.
2. The top `p` bits pick a register.
3. The number of leading zeros in the remaining bits + 1 is a "rank". Keep the
   register's running maximum rank.

Intuition: seeing a hash with `k` leading zeros suggests you've observed on the
order of `2^k` distinct items, because that pattern has probability `2^-k`.
Averaging the maxima across many registers (harmonic mean, with bias
corrections) turns that intuition into a stable estimate. A linear-counting
correction kicks in at low cardinality where many registers are still empty.

Measured: ≤2% relative error from 1k to 1M distinct keys, memory flat at 16 KB.

**Hashing.** FNV-1a (allocation-free over a string) finalized with a SplitMix64
mixing step. Raw FNV-1a has weak bit avalanche, which biases the register
distribution; the finalizer scrambles the output so the high and low 32-bit
halves are usable as near-independent hashes (this also feeds the Count-Min
double-hashing below).

## Count-Min Sketch + top-K — heavy hitters

Estimates per-key frequency in a fixed `d × w` counter grid. A key hashes to one
cell per row; increment all `d`, estimate by the **minimum** across rows. It can
overcount (hash collisions only ever add) but never undercounts — bounded by
`ε·N` with probability `1-δ`. The `d` row hashes come from one 64-bit hash via
Kirsch–Mitzenmacher double hashing (`h1 + i·h2`), so a lookup is one hash
regardless of depth.

A sketch alone can't *enumerate* the hot keys (it has no key list), so a small
bounded map tracks the current top-K: a key that outscores the weakest leader
evicts it. This is the standard "CMS + heap" heavy-hitters scheme — approximate
recall, but O(1) memory in the number of distinct keys.

Measured: recovers the true top-10 paths out of 50k distinct keys under a skewed
load; never undercounts.

## Bucketed histogram — latency percentiles

Latency goes into fixed exponential buckets (~20% apart, spanning ~50 µs to
~60 s). Percentiles are read by walking cumulative bucket counts and
interpolating within the crossing bucket. This is the same approach Prometheus
histograms use: O(1) update, no per-sample storage, and relative error bounded
by the bucket width.

Measured: p50 exact, p95 within ~1.8%, p99 within ~1% on a known distribution.

Why not t-digest? A t-digest gives tighter tail accuracy, but a geometric-bucket
histogram is simpler to implement correctly, trivially mergeable, and more than
accurate enough for operational percentiles — a deliberate
simplicity-over-precision tradeoff.

## What I'd add next

- **Sliding windows.** Today the sketches are lifetime-cumulative (plus a 60s
  RPS ring for the chart). Rotating a ring of per-minute sketches would give
  "last 5 minutes" views; HyperLogLog and Count-Min both merge cleanly, which is
  why `HyperLogLog.Merge` already exists.
- **Multiple backends.** Round-robin with passive health checks.
- **Rate limiting / WAF-lite.** The per-IP Count-Min estimate is already a
  cheap signal for token-bucket throttling of abusive clients.

## Testing

- Accuracy tests assert the estimators stay within their error bounds (HLL ≤2%,
  histogram percentiles within ~5%, Count-Min never undercounts, top-K recovers
  the true heavy hitters).
- An integration test drives concurrent traffic through the real proxy and
  control plane and checks the published snapshot end to end.
- The whole suite runs under `-race` in CI; the single-writer model keeps it
  clean.
