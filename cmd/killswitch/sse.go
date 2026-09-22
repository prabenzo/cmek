// Owner: Claude
package main

import (
	"fmt"
	"net/http"

	"github.com/prabenzo/cmek/internal/world"
)

// server holds what the handlers need; main builds exactly one and passes it to the mux (no package-level state).
type server struct {
	holder *world.Holder
}

// stream is GET /v1/stream: one SSE frame per metrics tick. The subscription is the viewer count: it is cancelled
// when the client goes away (r.Context) or the World stops (channel closed), so 1→0 pauses traffic.
func (s *server) stream(rw http.ResponseWriter, r *http.Request) {
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "no_world"})
		return
	}
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := w.Subscribe()
	defer cancel()
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	fmt.Fprint(rw, "retry: 1000\n\n")
	flusher.Flush()
	var n int64
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return // World stopped
			}
			n++
			if _, err := fmt.Fprintf(rw, "id: %d\ndata: %s\n\n", n, b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
