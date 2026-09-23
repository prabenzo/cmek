// Owner: Claude (reviewed by Ben)
package world

import (
	"fmt"
	"sync"
	"time"
)

// Holder owns "the current World" for main; instantiated once in main, never package-level. Every handler that
// touches a World goes through Acquire, Ensure or Current and defers the release it gets back: the World counts
// them (a per-World WaitGroup), and Stop waits ≤ StopTimeout for them before closing the database, so a Reset
// never closes the store under an ingest in flight [SC-F11]. The refcount is per World on purpose: a handler that
// outlives StopTimeout on an old World is logged and forgotten, and can never make a later rebuild wait again.
type Holder struct {
	mu  sync.Mutex
	p   Params
	d   Deps
	cur *World
	seq int
}

// NewHolder keeps p and d and copies them (plus a generated id) into every World it builds.
func NewHolder(p Params, d Deps) *Holder { return &Holder{p: p, d: d} }

func (h *Holder) now() time.Time {
	if h.d.Clock != nil {
		return h.d.Clock.Now()
	}
	return time.Now()
}

// build starts a fresh World under h.mu; a build failure is logged and leaves cur nil (handlers answer 503 no_world).
func (h *Holder) build() *World {
	h.seq++
	w, err := New(h.p, Deps{ID: fmt.Sprintf("w-%d", h.seq), Clock: h.d.Clock, Logger: h.d.Logger})
	if err != nil {
		if h.d.Logger != nil {
			h.d.Logger.Error("world build failed", "err", err)
		}
		h.cur = nil
		return nil
	}
	w.Start()
	h.cur = w
	return w
}

// Acquire returns the World for a new viewer, rebuilding first when there is none or when nobody has watched the
// current one for longer than IdleRebuild (a reviewer never opens someone else's leftover outage), and subscribes
// under the same mutex so two simultaneous reconnects cannot double-build. release ends the subscription and the
// handler's hold; it must be called when the handler returns.
func (h *Holder) Acquire() (w *World, snaps <-chan []byte, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur != nil && h.cur.Viewers() == 0 && h.cur.IdleFor(h.now()) > h.p.IdleRebuild {
		old := h.cur
		old.log.Info("idle rebuild", "idle_s", old.IdleFor(h.now()).Seconds())
		old.Stop()
		h.cur = nil
	}
	if h.cur == nil && h.build() == nil {
		return nil, nil, func() {}
	}
	w = h.cur
	ch, cancel := w.Subscribe()
	hold := w.hold()
	return w, ch, func() { cancel(); hold() }
}

// Ensure returns the current World, building one (paused, no viewer) if none exists; it never applies the idle
// rule, so curl works against a paused World. release must be called when the handler returns.
func (h *Holder) Ensure() (w *World, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil && h.build() == nil {
		return nil, func() {}
	}
	return h.cur, h.cur.hold()
}

// Current returns the current World or nil and a release (a no-op when nil); it never builds.
func (h *Holder) Current() (w *World, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return nil, func() {}
	}
	return h.cur, h.cur.hold()
}

// Reset stops the current World and builds a fresh one (POST /v1/reset): open streams end as their channels close
// and the page reconnects to the new id. The caller must not hold a release on the old World (Stop waits for it).
func (h *Holder) Reset() *World {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.cur; old != nil {
		old.log.Info("reset")
		old.Stop()
		h.cur = nil
	}
	return h.build()
}
