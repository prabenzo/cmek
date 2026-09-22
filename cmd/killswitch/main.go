// Owner: Claude
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prabenzo/cmek/internal/world"

	// x/time is first used in M3 (admission); the blank import keeps the module in the build cache.
	_ "golang.org/x/time/rate"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	p := world.Demo()
	if v, err := strconv.ParseInt(os.Getenv("SEED"), 10, 64); err == nil {
		p.Seed = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("KS_BASE_RATE"), 64); err == nil && v > 0 {
		p.BaseRate = v
	}
	if v := os.Getenv("KS_DB_DIR"); v != "" {
		p.DBDir = v
	}
	if v := os.Getenv("KS_SYNC"); v != "" {
		p.SyncMode = v
	}
	if v, err := strconv.Atoi(os.Getenv("KS_WAL_AUTOCHECKPOINT")); err == nil && v > 0 {
		p.WALAutocheckpoint = v
	}
	holder := world.NewHolder(p, world.Deps{Logger: logger})
	s := &server{holder: holder}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	w, release := holder.Ensure() // build the World at boot so /health has one
	release()
	if w == nil {
		logger.Error("no world at boot")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	s.routes(mux)
	routeUI(mux)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 0}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if cur := holder.Current(); cur != nil {
			cur.Stop()
		}
	}()
	logger.Info("killswitch listening", "port", port, "world", w.ID, "base_rate", p.BaseRate)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
}
