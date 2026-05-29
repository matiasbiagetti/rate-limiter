// Command demo runs a small IOL-style market-data API wired with the
// rate-limiter middleware, so the project runs out of the box:
//
//	go run ./cmd/demo                        # /api/* limited to 5 req/s per IP
//	go run ./cmd/demo -burst 10 -interval 2s
//
// It demonstrates the two things the library is for:
//
//   - Per-route limit policies: the market-data endpoints under /api/ get a
//     tight limit (they are the ones clients hammer), while browsing the index
//     gets a lenient one — selected per request via the PolicyFunc seam.
//   - A useful 429: a themed JSON body plus the X-RateLimit-* / Retry-After
//     headers, via the middleware's OnDenied hook.
//
// The server uses read/write timeouts and shuts down gracefully on Ctrl-C.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/biagettimati/rate-limiter/middleware"
	"github.com/biagettimati/rate-limiter/ratelimiter"
)

// quote is a mock market quote, just enough to make the demo concrete.
type quote struct {
	Symbol   string  `json:"simbolo"`
	Name     string  `json:"nombre"`
	Last     float64 `json:"ultimo"`
	ChangePc float64 `json:"variacionPct"`
	Currency string  `json:"moneda"`
}

// market is static mock data — a handful of well-known Argentine tickers and a
// CEDEAR — so the endpoint returns something recognisable without any backend.
var market = map[string]quote{
	"GGAL": {"GGAL", "Grupo Financiero Galicia", 5230.0, 1.84, "ARS"},
	"YPFD": {"YPFD", "YPF S.A.", 41250.0, -0.97, "ARS"},
	"PAMP": {"PAMP", "Pampa Energía", 3145.5, 2.31, "ARS"},
	"ALUA": {"ALUA", "Aluar Aluminio", 1087.0, 0.42, "ARS"},
	"AL30": {"AL30", "Bono Argentina 2030", 67890.0, 0.15, "ARS"},
	"AAPL": {"AAPL", "Apple Inc. (CEDEAR)", 18950.0, -0.63, "ARS"},
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	burst := flag.Int("burst", 5, "/api bucket capacity (maximum burst)")
	interval := flag.Duration("interval", time.Second, "time to refill the /api bucket to full")
	flag.Parse()

	apiRate := ratelimiter.Rate{Capacity: *burst, Interval: *interval}
	browseRate := ratelimiter.Rate{Capacity: 30, Interval: time.Minute}
	for _, r := range []ratelimiter.Rate{apiRate, browseRate} {
		if err := r.Validate(); err != nil {
			log.Fatalf("invalid rate: %v", err)
		}
	}

	// Eviction enabled: idle per-IP buckets are swept so memory stays bounded.
	lim := ratelimiter.New(ratelimiter.Options{
		SweepInterval: time.Minute,
		IdleTTL:       10 * time.Minute,
	})
	defer lim.Close()

	// Per-request policy: tight on the market-data API, lenient on browsing.
	policy := func(r *http.Request) ratelimiter.Rate {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			return apiRate
		}
		return browseRate
	}

	limit, err := middleware.New(middleware.Config{
		Limiter:  lim,
		Policy:   policy,
		OnDenied: http.HandlerFunc(denied),
	})
	if err != nil {
		log.Fatalf("configure middleware: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/quotes", quotesHandler)
	mux.HandleFunc("/", indexHandler(apiRate, browseRate))

	srv := &http.Server{
		Addr: *addr,
		// Composition: log the outermost view of each request, then rate-limit,
		// then route. The logger sees the final status, including 429s.
		Handler:      logRequests(limit(mux)),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("listening on %s — /api limited to %d per %s per client IP",
			*addr, apiRate.Capacity, apiRate.Interval)
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

// statusRecorder wraps a ResponseWriter to remember the status code, which the
// standard ResponseWriter does not expose for logging. It defaults to 200, the
// status net/http sends implicitly when a handler writes a body without calling
// WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request — method, target, client IP, final
// status, and latency. It wraps (composes around) the rate limiter so the
// status it logs already reflects any 429 the limiter produced.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%-4s %-30s from %-21s -> %d (%s)",
			r.Method, r.URL.RequestURI(), middleware.ClientIP(r),
			rec.status, time.Since(start).Round(time.Microsecond))
	})
}

// quotesHandler returns a single quote when ?simbolo= is given, otherwise the
// whole mock market. All responses also carry the X-RateLimit-* headers set by
// the middleware.
func quotesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if sym := strings.ToUpper(r.URL.Query().Get("simbolo")); sym != "" {
		q, ok := market[sym]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "simbolo_no_encontrado",
				"simbolo": sym,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(q)
		return
	}

	all := make([]quote, 0, len(market))
	for _, q := range market {
		all = append(all, q)
	}
	_ = json.NewEncoder(w).Encode(all)
}

// denied is the custom 429 response: a themed JSON body. The middleware has
// already set X-RateLimit-* and Retry-After, which we echo so the body is
// self-explanatory.
func denied(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             "limite_de_solicitudes",
		"mensaje":           "Demasiadas solicitudes. Reintentá más tarde.",
		"retryAfterSeconds": w.Header().Get("Retry-After"),
		"limite":            w.Header().Get("X-RateLimit-Limit"),
	})
}

// indexHandler serves a short help page describing the endpoints, the limits in
// effect, and the headers worth watching.
func indexHandler(api, browse ratelimiter.Rate) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, `IOL-style market data — rate limiter demo

Endpoints:
  GET /                      this page            (limit: %d per %s per IP)
  GET /api/quotes            all quotes           (limit: %d per %s per IP)
  GET /api/quotes?simbolo=GGAL   one quote

Every response carries:
  X-RateLimit-Limit       capacity for this route
  X-RateLimit-Remaining   tokens left after this request
On 429 it also sends:
  Retry-After             seconds until a token frees up

Try it:
  curl -i "http://localhost:8080/api/quotes?simbolo=GGAL"
  (send several /api/quotes requests quickly to trip the limit and see the 429)
`,
			browse.Capacity, browse.Interval, api.Capacity, api.Interval)
	}
}
