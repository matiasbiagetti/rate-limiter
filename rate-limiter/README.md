# Rate Limiter Middleware

An in-memory, token-bucket rate limiter exposed as `net/http` middleware, in Go.

## Quick start

```bash
cd rate-limiter
go build ./...
go test ./...
CGO_ENABLED=1 go test -race ./...   # concurrency correctness
go run ./cmd/demo                   # starts the demo API on :8080
```

## Design at a glance

- **Algorithm:** token bucket with lazy refill — bursts allowed, no per-key timers.
- **Core (`ratelimiter/`):** transport-agnostic; `Allow(key, rate) Decision`.
- **Middleware (`middleware/`):** injectable `KeyFunc` (limiting dimension) and
  `PolicyFunc` (per-request limit selection), `429` + `Retry-After` +
  `X-RateLimit-*` headers, and an optional `OnDenied` hook to customise the
  rejection body.
- **Concurrency:** single mutex, race-tested; sharding documented as the scaling path.
- **Memory:** idle buckets evicted by a background janitor.

Full rationale and trade-offs: [`DESIGN.md`](./DESIGN.md).

## Demo: an IOL-style market-data API

`go run ./cmd/demo` starts a small mock quotes API that exercises the library
end to end:

- `GET /api/quotes` — all quotes; `GET /api/quotes?simbolo=GGAL` — one quote.
- **Per-route limits:** `/api/*` gets a tight limit (the endpoints clients poll);
  the index page gets a lenient one — chosen per request via `PolicyFunc`.
- **Visible rate-limit signals:** every response carries `X-RateLimit-Limit` and
  `X-RateLimit-Remaining`; a denied request returns a themed JSON body plus
  `Retry-After`.
- **Request logging:** each request is logged (method, target, client IP, status,
  latency) by a logging middleware composed *around* the limiter — so a `429`
  shows up in the log just like any other response.

```bash
# See the headers on a successful request:
curl -i "http://localhost:8080/api/quotes?simbolo=GGAL"

# Trip the limit (default 5/s on /api): the 6th+ within a second returns 429.
go run ./cmd/demo -burst 3 -interval 1m   # easy to trip: 3 then blocked
```

A `429` looks like:

```
HTTP/1.1 429 Too Many Requests
X-RateLimit-Limit: 3
X-RateLimit-Remaining: 0
Retry-After: 60
Content-Type: application/json

{"error":"limite_de_solicitudes","limite":"3","mensaje":"Demasiadas solicitudes. Reintentá más tarde.","retryAfterSeconds":"60"}
```

The server uses read/write timeouts and shuts down gracefully on Ctrl-C.
