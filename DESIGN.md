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

Updates are **conservative**: only the cells holding a key's current minimum
are raised. The never-undercount guarantee is unchanged, but a light key that
shares cells with a heavy one picks up far less of its count. That is what
keeps innocent clients from being rate limited during a flood.

A sketch alone can't *enumerate* the hot keys (it has no key list), so a
min-heap of size k tracks the current leaders. The weakest leader sits at the
root, so deciding whether a new key gets in is O(1) and swapping it in is
O(log k). An earlier version scanned every leader on each miss, which made a
k=1024 tracker cost ~11.8 µs per request; the heap costs ~0.19 µs.

Measured: recovers the true top-10 paths out of 50k distinct keys under a skewed
load; never undercounts.

## Rate limiting — detect on every request, block lock-free

The first version flagged abusers from the top-K tracker once per second, which
had three problems: at most 1,024 IPs could ever be blocked (a botnet bigger
than k got through, and its members kept evicting each other); enforcement lagged
up to a second; and the window was fixed, so a client could send a full
allowance just before a reset and another just after.

Now:

- **Detection is per request.** The limiter's own Count-Min sketch returns the
  IP's estimate on every increment, so the engine compares it with the threshold
  right there. Any number of IPs can be caught, tracked or not.
- **The window slides.** Two sketches hold the current and previous fixed
  windows; the estimate is `current + previous × (fraction of previous still in
  the window)`. This is the approximation Cloudflare describes for its rate
  limiter: O(1) memory, and burst-straddling no longer doubles the allowance.
- **The blocklist is a `sync.Map` with expiries.** The single analytics
  goroutine is the only writer; proxy goroutines read it lock-free (~1.7 ns), so
  a block takes effect on the next request. Blocked requests are still counted,
  so a client that keeps hammering stays blocked; once it stops, the window
  drains and the entry expires.
- **The blocklist is capped** (65,536 entries ≈ 6.5 MB). Offenders beyond the
  cap are counted in `block_overflow`. A larger distributed attack needs
  different tools (subnet limits, global load shedding, traffic fingerprinting),
  not a longer per-IP list.
- **The sketch is wide and shallow** (4 × 16,384) so collisions stay rare even
  with thousands of heavy IPs: with 5,000 abusive IPs, 0 of 2,000 light clients
  were blocked in the test.

## Client IPs — trust only what an edge vouches for

`X-Forwarded-For` is just a header; any client can send `X-Forwarded-For:
1.2.3.4`. The first version trusted its first hop, which let one client pose as
thousands, both to dodge the limiter and to inflate visitor counts. Vantage now
uses the TCP peer address unless `-ip-header` names a header that a trusted
edge overwrites (`Fly-Client-IP` on Fly.io).

## Site analytics — humans, bots, scanners

Each request is classified from its User-Agent and path:

- **Scanner:** paths only vulnerability scanners request on a site like this
  one (`.php`, `/wp-login`, `/.env`, `/.git`). Checked first, because scanners
  often claim to be browsers.
- **Bot:** self-identified crawlers, preview fetchers, monitors and HTTP
  libraries, or an empty User-Agent.
- **Human:** everything else. A determined bot can pass as a browser; the goal
  is to remove the honest background noise, which is most of it.

A **page view** is a human GET for a document (not `/_next/` assets, images or
fonts) that succeeded and wasn't a prefetch (`Next-Router-Prefetch`,
`Sec-Purpose: prefetch`). Referrers are reduced to bare hosts, and internal
navigation is dropped, so the referrer table answers "where did visitors come
from".

## Daily history and the Markdown report

A per-day row (visitors, page views, bots, scanner probes, rate-limited) rolls
over at midnight in `-tz`. Visitors per day comes from a separate HyperLogLog
that resets at midnight. With `-state`, the history and today's registers
(16 KB) are written every minute and on shutdown, via a temp file and rename
so a crash never leaves a truncated file. A mid-day deploy keeps the day's
counts.

`server.Markdown` renders the snapshot as GitHub-flavoured tables. It contains no
IP addresses, and every cell is escaped (inline code, `|` escaped, backticks and
newlines removed) because paths and referrers are attacker-controlled. The report
is written to `-report-file` and, with `-report-path`, served on the data plane
behind HTTP Basic auth with a constant-time password comparison. Requests to
that path are never proxied or counted.

## Origin connection pool

Go's default transport keeps 2 idle connections per host. A proxy talks to one
host, so under concurrency it was opening and discarding an origin connection
for nearly every request, adding a handshake each time and filling the OS with
`TIME_WAIT` sockets. In a 20,000-request local test that exhausted Windows'
ephemeral ports (16,570 sockets in `TIME_WAIT`) and connections began failing.
A pool of 256 idle connections per host brought it to 31 sockets and raised
throughput from 3,921 to 18,122 req/s.

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

- **Sliding analytics windows.** Top pages and referrers are cumulative since
  start. Rotating a ring of per-minute sketches would give "last 5 minutes"
  views; HyperLogLog and Count-Min both merge cleanly, which is why
  `HyperLogLog.Merge` already exists.
- **Subnet limits and global load shedding** for distributed attacks that stay
  under the per-IP threshold.
- **Multiple backends.** Round-robin with passive health checks, then routing by
  request content (for example, sending LLM requests that share a prompt prefix
  to the replica that already has it cached).

## Testing

- Accuracy tests assert the estimators stay within their error bounds (HLL ≤2%,
  histogram percentiles within ~5%, Count-Min never undercounts, top-K recovers
  the true heavy hitters, and its heap index stays consistent).
- Rate-limiter tests cover a 5,000-IP botnet (all blocked, false positives
  ≤1%), a burst straddling a window boundary, expiry, the blocklist cap, and
  flat memory over 500k distinct IPs. A fake clock makes them instant.
- Proxy tests check that a forged `X-Forwarded-For` is ignored by default.
- Daily-history tests cover midnight rollover in a real time zone and restoring
  state after a restart on the same day and on the next day.
- An integration test drives concurrent traffic through the real proxy and
  control plane and checks the published snapshot end to end.
- The whole suite runs under `-race` in CI; the single-writer model keeps it
  clean.
