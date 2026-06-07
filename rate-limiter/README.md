# Rate Limiter Middleware

An in-memory, token-bucket rate limiter exposed as `net/http` middleware, in Go.

## Quick start

```bash
cd rate-limiter
go build ./...                            # compile
go test ./...                             # unit + integration tests
go test -race ./...                       # race detector (needs cgo + a C compiler)
go test -bench . -benchmem ./ratelimiter  # hot-path benchmarks
go run ./cmd/demo                         # start the demo API on :8080
```

## Design at a glance

- **Algorithm:** token bucket with lazy refill (bursts allowed, no per-key timers).
- **Core (`ratelimiter/`):** transport-agnostic; `Allow(key, rate) Decision`.
- **Middleware (`middleware/`):** injectable `KeyFunc` (limiting dimension) and
  `PolicyFunc` (per-request limit selection), `429` + `Retry-After` +
  `X-RateLimit-*` headers, and an optional `OnDenied` hook to customise the
  rejection body.
- **Concurrency:** single mutex, race-tested; sharding documented as the scaling path.
- **Memory:** idle buckets evicted by a background janitor.

Full rationale and trade-offs: [`DESIGN.md`](./DESIGN.md).

## Demo: a market-data API

`go run ./cmd/demo` starts a small mock quotes API that exercises the library end to end.

### Endpoints

- `GET /api/quotes` — all quotes (JSON).
- `GET /api/quotes?symbol=GGAL` — a single quote.
- `GET /` — a plain-text help page describing the limits and headers.

### How the limits work

The demo applies two limits, chosen per request by **route family**:

- **`/api/*` (strict)** — configured by the run flags below. Default: 5 requests,
  refilled over 1 second.
- **Everything else / browsing (lenient)** — **hardcoded** in the demo at
  **30 requests per minute** (`Rate{Capacity: 30, Interval: time.Minute}` in `main.go`).
  There is no flag for it; it is a fixed value.

The key is `clientIP|routeFamily`, so each family has its **own independent bucket**:
saturating `/api` leaves browsing (`/`) unaffected, and vice versa.

### Run parameters (flags)

All flags are optional: `go run ./cmd/demo` with none uses the defaults below.

| Flag | Default | What it sets |
|---|---|---|
| `-addr` | `:8080` | the address the server listens on |
| `-burst` | `5` | the `/api` bucket **capacity** (the maximum burst) |
| `-interval` | `1s` | the time to refill the `/api` bucket to full |

`-burst` and `-interval` together form the `/api` quota: `Rate{Capacity: burst, Interval: interval}`.
For example `-burst 3 -interval 1m` means "3 requests, refilled over a minute" (about 3/min with a
burst of 3). `-interval` accepts Go durations such as `500ms`, `2s`, `1m`, `1h`.

### Testing it

```bash
# 1. Start the server with an easy-to-trip /api limit (3, refilled over a minute):
go run ./cmd/demo -burst 3 -interval 1m

# 2. In another terminal, see one response with its status line, headers and body:
curl -i "http://localhost:8080/api/quotes?symbol=GGAL"

# 3. Fire several /api requests to trip the limit (the 4th returns 429):
for i in $(seq 1 5); do echo "--- request $i ---"; curl -s -i http://localhost:8080/api/quotes; echo; done

# 4. Confirm browsing is unaffected (different family, its own bucket): still 200
curl -i http://localhost:8080/
```

A `429` looks like:

```
HTTP/1.1 429 Too Many Requests
X-RateLimit-Limit: 3
X-RateLimit-Remaining: 0
Retry-After: 60
Content-Type: application/json

{"error":"rate_limited","limit":"3","message":"Too many requests, please retry later.","retryAfterSeconds":"60"}
```

The server uses read/write timeouts and shuts down gracefully on Ctrl-C.
