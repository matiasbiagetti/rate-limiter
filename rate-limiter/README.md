# Rate Limiter Middleware

An in-memory, token-bucket rate limiter exposed as `net/http` middleware, in Go.

## Quick start

```bash
cd rate-limiter
go build ./...
go test ./...
go test -race ./...
go run ./cmd/demo      # starts the demo server
```

## Design at a glance

- **Algorithm:** token bucket with lazy refill — bursts allowed, no per-key timers.
- **Core (`ratelimiter/`):** transport-agnostic; `Allow(key, rate) Decision`.
- **Middleware (`middleware/`):** injectable `KeyFunc` (limiting dimension) and
  `PolicyFunc` (per-request limit selection); returns `429` + `Retry-After` and
  `X-RateLimit-*` headers.
- **Concurrency:** single mutex, race-tested; sharding documented as the scaling path.
- **Memory:** idle buckets evicted by a background janitor.

Full rationale and trade-offs: [`DESIGN.md`](./DESIGN.md).

> Status: scaffold (compiling stubs). Core logic marked with `TODO(impl)` is in progress.
