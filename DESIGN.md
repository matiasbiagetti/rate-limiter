# DESIGN: Rate Limiter Middleware

> My implementation of the "Design a rate limiter" problem (Alex Xu, *System
> Design Interview* Vol. 1). This document records **what** I built, the
> **architectural choices** behind it, the **trade-offs** I weighed, and **how I
> used AI**. 

## 0. Build, run, and test

Everything lives under `rate-limiter/` (a self-contained Go module). From there:

```bash
go build ./...                 # compiles
go test ./...                  # all tests pass
go test -race ./...            # race detector
go test -bench . ./ratelimiter # hot-path benchmarks
go run ./cmd/demo              # runnable demo API on :8080
```

> Note: `-race` is built on a C runtime (ThreadSanitizer), so it needs cgo and a C
> compiler. That's the default on Linux/macOS; on Windows you may need
> `CGO_ENABLED=1` and a gcc in `PATH`.

## 1. Problem & scope

A rate limiter caps how many requests a client may make in a time window,
shielding a service from abuse and overload. I built it as an **HTTP middleware**
backed by an **in-memory token-bucket** limiter.

**In scope (what I committed to and finished):**

- Throttle by a **pluggable key** (client IP, API token, etc.): the limiting
  dimension is injected, not hardcoded.
- **Flexible, per-request limit configurations**: the applicable limit is chosen
  per request (per route / per tier / per plan), not fixed globally.
- Standard rejection semantics: **429 Too Many Requests** with `Retry-After` and
  `X-RateLimit-Limit` / `X-RateLimit-Remaining` headers.
- **Bounded memory** under floods of unique keys (eviction of idle buckets).
- **Concurrency-safe** under real parallel load, proven with `-race` tests.

**Out of scope, deliberately** (the "Prototype vs production" section covers the production view): distributed /
multi-node coordination (Redis), dynamic rule reloading or an admin API, and
per-client persistence across restarts. I treated drawing these boundaries as part
of the work; the brief explicitly rewards avoiding overengineering as the challenged specified, so I scoped to
something I could finish, test, and fully defend.

## 2. Why Go

A rate-limiter middleware lives in the hot path of every request and mutates
shared per-key state concurrently, so the language's concurrency story matters more
than syntax. Go fits this exactly: middleware is idiomatic
(`func(http.Handler) http.Handler`); goroutines model concurrent load naturally;
and the built-in **race detector** (`go test -race`) lets me *prove* the absence of
data races rather than argue it. It also ships a single static binary, which makes
deployment trivial. Python or a single-threaded event loop would have hidden
the very concurrency this problem is about; Java would have worked with more
ceremony. 

## 3. Architecture & module structure

```
ratelimiter/   core domain: tokenBucket, Limiter, Clock, Options, eviction. No HTTP.
middleware/    net/http adapter. Depends on ratelimiter; never the reverse.
cmd/demo/      a small server that proves the thing runs out of the box.
```

The most important boundary in a *middleware* problem is transport (HTTP) versus
domain (the token-bucket logic). I made that boundary a **package boundary**, which
forces a one-way dependency (`middleware` imports `ratelimiter`)
and keeps the core pure and unit-testable without `httptest`. Each package is a **deep module**: `tokenBucket` hides
the refill arithmetic, `Limiter` hides concurrency and eviction behind a single
method `Allow(key, rate) Decision`, and `middleware` is a thin adapter. Collapsing
into one package would let `net/http` leak into the core; splitting into one-file
packages would create shallow modules. Two packages is the minimum that expresses
the real boundary.

## 4. Algorithm: token bucket

Each key owns a bucket of up to `Capacity` tokens, refilled continuously to full
over one `Interval`. A request consumes one token; if none remain, it's denied.
Tokens are recomputed **lazily on access** from elapsed time, so there is no per-key
timer or background refill goroutine, just arithmetic when a key is hit, which is
what keeps it cheap at scale.

Why token bucket over other algorithms:

| Algorithm | Why I didn't choose it |
|---|---|
| Fixed window counter | Boundary-burst bug: up to 2x the limit across a window edge. |
| Sliding window log | Stores every request timestamp, so memory grows with traffic. |
| Sliding window counter | More accurate but more state/arithmetic for little gain at this scope. |
| Leaky bucket | Smooths output to a constant rate; I want to *permit* bursts, not flatten them. |

Token bucket is the real-world default, handles bursts, and forms a clean deep
module. I accept its minor accuracy trade-off versus a sliding-window counter.

**`float64` internally, `int` at the boundary.** Tokens are a `float64` because
fractional tokens accrue between requests (at 1 token / 100 ms, 50 ms means 0.5
tokens). Integers would either drop that progress (making the effective rate slower
than configured) or round it up (more permissive). The exposed `Remaining` is an
`int` (floored), because a partial token can't satisfy a request and the
`X-RateLimit-Remaining` header is an integer. I also multiply before dividing in the
refill/retry math to avoid float reciprocal error, so `Retry-After` lands on exact
values (AI suggested this, pretty interesting).

## 5. Concurrency

A single `sync.Mutex` guards the bucket map and the per-bucket read-modify-write,
encapsulated inside `Limiter`. The critical detail is that the lock is held across
the **entire** `bucket.allow` call (refill, check, consume): that's the operation
that, if interleaved, would let two requests double-spend the same token. The
same lock also covers the map, because concurrent map writes panic in Go.

**Sharding the map by key hash is the documented scaling path**, taken only if a
benchmark shows contention (measured, not assumed). Because the lock is internal,
that change wouldn't touch the public API.

I prove correctness rather than assert it: `TestLimiter_ConcurrentAllow_NoOverAdmission`
hammers one key from 64 goroutines (3,200 attempts) with a fixed clock; under
`-race` the detector reports nothing **and** admissions equal capacity exactly. The
`BenchmarkAllow_*` benchmarks measure the single-mutex hot path so the "shard only if
measured" stance rests on numbers. These benchmarks were entirely an AI suggestion; I
hadn't planned on adding them, but they seemed a worthwhile extra, so I adopted them.
`go test -bench . -benchmem` reports, per benchmark, the time per call (`ns/op`) and the
memory cost (`B/op` and `allocs/op`). `BenchmarkAllow_Serial` is the uncontended cost;
`BenchmarkAllow_Parallel` (via `RunParallel`) is the cost when many goroutines hammer one
hot key, the worst case for a single mutex. I don't quote machine-specific figures here
because they depend on the hardware; the value is in what the output lets me compare.
`allocs/op` should be 0, which means the hot path puts no pressure on the garbage collector,
and the gap between the parallel and serial `ns/op` is precisely the lock-contention cost.
That gap is the signal for the sharding decision: while the contended number stays well
within budget a single mutex is enough, and if a benchmark showed it degrading, that is when
I would shard.

## 6. Configuring limits (policies)

A limit is a `Rate{Capacity, Interval}`. I pass the rate to `Allow(key, rate)` **per
call** rather than fixing it on the `Limiter`. That single seam is what enables
flexible, per-request limits: the `middleware` exposes a `PolicyFunc(*http.Request)
Rate` (which rate applies) and a `KeyFunc(*http.Request) string` (who is counted),
both ordinary functions the caller supplies. Because they receive the whole request,
the limiting criteria can be anything (by IP, by API token, by route, by HTTP
method, by client tier resolved at runtime, or any combination) without changing the
library. `FixedRate` covers the common single-limit case.

**Applying policies**: I apply one `Limiter` wrapped around
the whole router and let the `PolicyFunc` pick the rate per request. The demo keys by `IP|routeFamily` so per-family
buckets stay independent.

**Compound limits.** Because the middleware is a plain `func(http.Handler) http.Handler`,
several limiters compose by stacking: one keyed by IP and another keyed by user and
endpoint enforce both at once (a request must pass each). That covers multi-dimensional
limits with no special support in the core. This differs from combining dimensions into a
single key, which is one rule over a composite key rather than two independent rules.

**Customizable rejection.** The middleware takes an optional `OnDenied http.Handler`
(default: a plain-text 429). It runs with the `X-RateLimit-*` and `Retry-After`
headers already set, so a caller can render a JSON body or whatever they need. It's
opt-in with a sane default, a real feature of any limiter middleware, not gold-plating.

**Dynamic limits / hot reload (a production capability I designed for, not built).**
Because `Policy` is just a function I supply, the limits don't have to be static: a
production version could change them without a redeploy. What I want to call out is that
I thought through not only *that* hot reload is useful, but *how* to do it without hurting
the hot path. Reading the source (a config service or a DB) on every request would add
per-request latency and load that source, so that is exactly what I would avoid. Instead,
a background goroutine refreshes an in-memory snapshot on an interval (or on a change
event), and `Policy` reads that snapshot: a wait-free load,
no lock and no network on the request path, so the per-request cost stays in nanoseconds
rather than a round-trip. The trade-off is eventual consistency (a change takes effect
after the refresh interval), which is fine for tuning limits. I left it out of the
prototype because flags cover the demo, but the per-call `Policy` seam already makes it a
drop-in addition.

## 7. Error handling

In-memory limiting can't fail at runtime (no I/O), so I **define errors out of
existence**: `Allow` returns a `Decision` value, not `(Decision, error)`, and `New`
can't fail either. Configuration validity is a boundary concern: `Rate.Validate`
rejects non-positive capacities/intervals where limits are defined, and
`middleware.New` returns `ErrNoLimiter`/`ErrNoPolicy` for a missing dependency at
startup. This keeps the request path free of error-handling noise.

Validation runs in two layers. The loud one is at startup: `Rate.Validate` is called
where limits are defined (the demo turns an invalid rate into a `log.Fatalf`, so the app
refuses to start). The quiet one is a defensive backstop inside `Allow`: if an invalid
rate still reaches it at runtime (say, a buggy dynamic policy), it **fails closed**
(denies, stores no bucket) rather than dividing by a zero interval, which would produce
`+Inf` tokens and *silently disable the limiter*. The backstop itself does not log (the
core stays logging-free by design); its only signal is a spike in 429s that an error-rate
alert would catch. For a protective control, failing closed is the conservative choice:
blocked traffic is operationally loud, whereas letting everything through defeats the
limiter and can go unseen. (This is the opposite of the right answer for a network
dependency, discussed in the scaling section below, where fail-*open* usually wins; the
rule is not dogmatic, it depends on what you're protecting.)

## 8. Memory safety / eviction

Each distinct key creates a bucket, so an unbounded set of unique keys (e.g. spoofed
IPs) is a memory-exhaustion vector (the defense mechanism itself becoming an attack
surface). A background **janitor** removes idle buckets, opt-in via `SweepInterval > 0`
(starting a goroutine should be the caller's explicit choice) and stoppable via an
idempotent `Close`.

The subtlety is **which** bucket is safe to drop. Evicting a half-drained bucket would
reset its client's limit on recreation (it starts full), which an attacker could abuse
by waiting out the janitor. So eviction is **lossless**: a bucket is removed only after
it's been idle for at least its own `Interval`, meaning it has already refilled to
capacity, so recreating it yields an identical full bucket and removing it changes
nothing observable. `IdleTTL` is extra grace on top of that correctness floor. A naive
fixed TTL shorter than the interval would be the bug.

**Cost of the sweep.** Each sweep is `O(N)` over the map and holds the lock while it
runs, but it runs on the interval (e.g. once a minute), not per request, and does trivial
work per entry. CPU is negligible; the real concern only appears at millions of keys,
where holding the lock during a long scan could cause a latency spike. The scaling paths
are sharding (per-shard sweep), sampled/incremental sweeping (Redis-style), or a
`lastSeen`-ordered structure, none of which I implemented, because that would be premature.

## 9. Testing strategy

I drove behaviour with tests first and made time deterministic by injecting a `Clock`
(`ManualClock` in tests, advanced by hand: no `time.Sleep`, so tests are fast and not
flaky). Coverage of the core logic:

- **Refill math** with the manual clock: starts full, exhaustion, refill proportional to
  elapsed time, never exceeds capacity, exact `Retry-After`.
- **Concurrency** under `-race`: no data race, no over-admission.
- **Eviction**: `sweep`/`evictIdle` driven deterministically by the manual clock, plus a
  real-time test that the background janitor evicts on its own.
- **Rate changes**: adopting a larger rate, and a smaller one.
- **Fail-closed**: an invalid rate is denied and creates no bucket.
- **Lifecycle**: `Close` is idempotent.
- **Middleware** (`httptest`): 200 vs 429, header correctness, per-IP keying, config
  validation, and a custom `OnDenied` body.
- **Benchmarks** on the hot path to back any performance claim.

## 10. Observability

Logging and metrics are cross-cutting concerns, so the library doesn't emit them itself,
because that would couple it to a format and destination. It exposes seams instead:

- **Access log by composition.** A logging middleware wraps the limiter
  (`logRequests(limit(mux))` in the demo); it sees the final status, 429s included.
  Swap it for `slog`/JSON without touching the limiter.
- **Per-decision detail by hook.** A generic access log doesn't know which key was
  denied or why; an `OnDenied` (or a future `OnDecision`) hook lets the caller log that
  detail with their own logger.
- **Metrics surfaced today.** `Limiter.Len()` (the number of tracked keys, a memory signal)
  and the `X-RateLimit-*` response headers. Full aggregate counters are a production addition
  I left out on purpose; the approach and its cardinality pitfall are in the production section.

## 11. Deployment & defensive design

The service is a single static Go binary: copy and run, no runtime to install. The demo
already includes production hygiene: server `ReadTimeout`/`WriteTimeout`/`IdleTimeout`
(so slow clients can't hang it) and **graceful shutdown** on `SIGINT` (drain in-flight
requests with a bounded context, then close).

Also note where "defensive design" lives in an in-memory limiter: the classic
network-resilience items (timeouts, connection pools, circuit breakers) only apply to an
external dependency, which I don't have. So defensiveness here is **memory safety
(eviction), concurrency correctness, clock handling, and fail-closed validation**, and
the network items move to the Redis path in the scaling section.

## 12. Scaling: the distributed (Redis) path

A single node can't share state across replicas: with N replicas behind a load balancer
and per-node in-memory buckets, a client could get up to Nx the intended limit. The fix
is shared state. I'd put a `Store` interface behind `Limiter` with a Redis implementation
that runs the token-bucket read-modify-write (atomicity is
essential, or replicas double-spend again). A network dependency is where the defensive
machinery earns its place: per-call **timeouts**, a right-sized **connection
pool**, a **circuit breaker**, and an explicit **fail-open vs fail-closed** policy (usually
fail-open with a local fallback, so the limiter never takes the service down).

On eviction, a managed Redis (like ElastiCache in AWS) **reduces the work, not the problem**:
Redis expires keys by TTL itself
and adds `maxmemory` policies and monitoring, so I'd no longer run a janitor in-process. But
the key cardinality doesn't vanish; it moves to Redis, I still set a correct TTL, still size Redis, and now pay a network round-trip
per request. The in-memory core's `Allow(key, rate)` shape is already compatible with
hiding a `Store` behind it, so this is additive, not a rewrite. Across multiple services,
keys are namespaced (`service:client`); separate Redis instances are about isolation, not
collision (the prefix already prevents that).

## 13. Prototype vs production

To be explicit about which choices are scoped to this exercise versus what I'd ship, so a
reviewer can tell a deliberate simplification from a final decision:

| In the prototype | Why here | In production |
|---|---|---|
| In-memory, single node | finishable, testable, no deps | Redis / shared store for a global limit across replicas |
| Key from `RemoteAddr` | the demo has no proxy | parse a *trusted* `X-Forwarded-For` behind a LB (spoofable raw) |
| Policy by route family (api/web, path-based) | the demo has no auth, so no client tier to read | by client tier from the authenticated identity (JWT claim / session) |
| `log.Printf` access log | readable, dependency-free | structured logging (`slog`/JSON) plus metrics via the hook |
| Limits via flags | run and test instantly | config from file/env/control-plane, ideally hot-reloadable |
| No auth plus static mock data | the demo is just a vehicle | real auth and a real backend |
| Metrics: `Len()` plus `X-RateLimit-*` headers | enough to watch the limiter in the demo | aggregate counters (`denied_total` by route/tier), never labeled by the raw key |

**On metrics.** The production approach is aggregate counters (`denied_total`, `allowed_total`)
labeled only by bounded dimensions like route or tier, never by the raw key: one series per key
would be a cardinality DoS, the same unbounded problem eviction solves on the data side. Per-key
denial detail with timestamps belongs in logs/events, not a metric; a bounded top-N (count-min
sketch or a fixed-size LRU) covers an offender ranking.

Two clarifications so this doesn't read as "everything is a shortcut." First, the
testability-driven designs (injected `Clock`, the `sweep`/`evictIdle` split) are **kept**
in production: they're good design, not compromises (only `ManualClock` is test-only).
Second, plenty is production-grade as-is: the algorithm, the module structure, concurrency
correctness, error handling, the `KeyFunc`/`PolicyFunc`/`OnDenied` seams, and the lossless
eviction rule. The single mutex and `O(N)` sweep are "simple until a benchmark says
otherwise", a *measured-scale* decision, not a demo shortcut.

## 14. How I used AI

I used an AI assistant throughout, under a **spec-first, human-reviewed** workflow designed
to keep my understanding ahead of the code:

- **The decisions were mine, made before code.** Everything in this document (token bucket
  over the alternatives, in-memory scope, one mutex with sharding deferred, lossless
  eviction, errors defined out of existence, per-call `Rate` for flexible policies) I
  reasoned through and agreed before implementation, not reverse-engineered from generated
  code.
- **Behaviour was pinned as tests first.** For each component I reviewed the test cases (the
  executable spec) before any implementation existed; the AI then wrote code to make them
  pass. The bucket math, the concurrency guarantee, and the eviction rule were all driven
  this way.
- **Every block is mine to defend.** I read each file and kept only code I understand and/or
  makes sense to me for this feature.
- **On the comments, so the density isn't misread.** Exported types and functions carry doc
  comments because that is standard Go convention: `go doc` and linters expect a doc comment on every exported identifier, and they document the public
  API. That density is not a sign of AI-written code I can't explain. The few comments *inside*
  functions are hand-written intent notes on the non-obvious lines (a few of them AI-suggested,
  like the precision trick in the refill math): I keep the *why* next to the code so I can
  explain those subtle parts on request, not because I can't.
- **A concrete example of adopting an AI suggestion: the injected `Clock`.** The
  dependency-injection seam for time (the `Clock` interface, with `realClock` in production
  and `ManualClock` in tests) was a pattern the assistant proposed. I adopted it on purpose
  because it makes the time-dependent logic (token refill and eviction) **deterministically
  testable**: a test advances a `ManualClock` by hand instead of calling `time.Sleep`, so the
  suite is fast and never flaky, while production simply wires the real clock. The cost is
  tiny (a one-method interface and a test-only `ManualClock`), I understood the trade before
  taking it, and I can explain every line; the how-it-works lives next to the code in
  `clock.go`. I mention it explicitly as an example of the workflow: the AI suggested, I
  evaluated and owned it.
- **Verification gated everything.** `go test ./...`, `go test -race ./...`, and the
  no-over-admission test are the trust-but-verify gate on all generated code, keeping "code
  I can't explain" at zero.
