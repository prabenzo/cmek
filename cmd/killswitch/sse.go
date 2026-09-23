// Owner: Claude
package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prabenzo/cmek/internal/world"
)

// server holds what the handlers need; main builds exactly one and passes it to the mux (no package-level state).
type server struct {
	holder   *world.Holder
	deadline sync.Once // logs SetWriteDeadline's ErrNotSupported once
}

// stream is GET /v1/stream: one SSE frame per metrics tick. The subscription is the viewer count: it is cancelled
// when the client goes away (r.Context), the World stops (channel closed) or the stream reaches StreamMaxAge (the
// handler says "event: reconnect" and ends it cleanly before Cloud Run's 60-minute cut; EventSource reconnects
// within retry and the same World resumes), so 1→0 pauses traffic. Every frame is written under a
// StreamWriteTimeout deadline, so a half-open client is detected within one deadline [SC-F11].
func (s *server) stream(rw http.ResponseWriter, r *http.Request) {
	w, ch, release := s.holder.Acquire()
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
	rc := http.NewResponseController(rw)
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	fmt.Fprint(rw, "retry: 1000\n\n")
	flusher.Flush()
	var maxAge <-chan time.Time
	if w.P.StreamMaxAge > 0 {
		maxAge = time.After(w.P.StreamMaxAge)
	}
	var n int64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-maxAge:
			fmt.Fprint(rw, "event: reconnect\ndata: {}\n\n")
			flusher.Flush()
			return
		case b, ok := <-ch:
			if !ok {
				return // World stopped
			}
			n++
			if w.P.StreamWriteTimeout > 0 {
				if err := rc.SetWriteDeadline(time.Now().Add(w.P.StreamWriteTimeout)); err != nil && errors.Is(err, http.ErrNotSupported) {
					s.deadline.Do(func() { w.Log().Warn("stream: write deadline unsupported by this ResponseWriter") })
				}
			}
			if _, err := fmt.Fprintf(rw, "id: %d\ndata: %s\n\n", n, b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
