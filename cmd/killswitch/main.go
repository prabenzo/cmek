// Owner: Claude
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prabenzo/cmek/internal/world"

	// Blank imports warm the module and build caches in the M0 image; M1 uses all three.
	_ "golang.org/x/sync/singleflight"
	_ "golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	burnMs := 50
	if v, err := strconv.Atoi(os.Getenv("KS_TICK_BURN_MS")); err == nil && v >= 0 {
		burnMs = v
	}
	p := world.Demo()
	holder := world.NewHolder(p)
	s := &server{holder: holder, burn: time.Duration(burnMs) * time.Millisecond}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	w, release := holder.Ensure()
	defer release()
	go s.runTicker(ctx, w, p.SnapshotInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/stream", s.stream)
	routeUI(mux)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 0}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("killswitch %s listening on :%s (tick burn %d ms)", w.ID, port, burnMs)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
