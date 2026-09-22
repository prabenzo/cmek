// Owner: Claude
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prabenzo/cmek/internal/world"
)

// server holds what the handlers need; main builds exactly one and passes it to the mux (no package-level state).
type server struct {
	holder  *world.Holder
	burn    time.Duration // KS_TICK_BURN_MS: busy-burn per tick so the Cloud Run ticker check measures a World-sized load
	viewers atomic.Int32
}

// runTicker burns s.burn of CPU every interval and then advances the World's tick counter, until ctx ends.
// A bare atomic increment could keep ticking under an idle CPU quota, so the burn is what makes the M0 check discriminating.
// M1 deletes this: the metrics tick owns the counter from then on.
func (s *server) runTicker(ctx context.Context, w *world.World, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for start := time.Now(); time.Since(start) < s.burn; {
			}
			w.Tick()
		}
	}
}

// stream is GET /v1/stream: one JSON frame per interval with the current tick and world id.
// It returns when the client goes away (r.Context) or a write fails, so the viewer count drops on close.
func (s *server) stream(rw http.ResponseWriter, r *http.Request) {
	w, release := s.holder.Ensure()
	defer release()
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	flusher.Flush()
	s.viewers.Add(1)
	defer s.viewers.Add(-1)
	t := time.NewTicker(w.P.SnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if _, err := fmt.Fprintf(rw, "data:{\"tick\":%d,\"world\":%q}\n\n", w.Ticks(), w.ID); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// health is GET /health: liveness plus the tick counter the M0 check reads.
func (s *server) health(rw http.ResponseWriter, r *http.Request) {
	w := s.holder.Current()
	rw.Header().Set("Content-Type", "application/json")
	if w == nil {
		rw.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(rw).Encode(map[string]string{"error": "no_world"})
		return
	}
	_ = json.NewEncoder(rw).Encode(w.Health(time.Now(), int(s.viewers.Load()), int(s.burn/time.Millisecond)))
}
