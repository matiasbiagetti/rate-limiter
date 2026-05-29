// Command demo runs a tiny HTTP server wired with the rate-limiter middleware,
// so the project runs out of the box:
//
//	go run ./cmd/demo                       # default: 5 requests / second per IP
//	go run ./cmd/demo -burst 10 -interval 2s
//
// It is intentionally minimal — just enough to exercise the middleware end to
// end and to serve as living usage documentation. The server uses read/write
// timeouts and shuts down gracefully on Ctrl-C.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/biagettimati/rate-limiter/middleware"
	"github.com/biagettimati/rate-limiter/ratelimiter"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	burst := flag.Int("burst", 5, "bucket capacity (maximum burst)")
	interval := flag.Duration("interval", time.Second, "time to refill the bucket to full")
	flag.Parse()

	rate := ratelimiter.Rate{Capacity: *burst, Interval: *interval}
	if err := rate.Validate(); err != nil {
		log.Fatalf("invalid rate: %v", err)
	}

	// Eviction enabled: idle per-IP buckets are swept so memory stays bounded.
	lim := ratelimiter.New(ratelimiter.Options{
		SweepInterval: time.Minute,
		IdleTTL:       10 * time.Minute,
	})
	defer lim.Close()

	limit, err := middleware.New(middleware.Config{
		Limiter: lim,
		Policy:  middleware.FixedRate(rate),
	})
	if err != nil {
		log.Fatalf("configure middleware: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello\n"))
	})

	srv := &http.Server{
		Addr:         *addr,
		Handler:      limit(mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("listening on %s — limit %d per %s per client IP", *addr, rate.Capacity, rate.Interval)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Block until interrupted, then drain in-flight requests before exiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("stopped")
}
