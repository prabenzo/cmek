// Owner: Claude (reviewed by Ben)
package world

import (
	"fmt"
	"sync"
)

// Holder owns "the current World" for main; instantiated once in main, never package-level.
// M1 ships Ensure and Current; M5 adds Acquire's idle rule, Reset and the handler refcount.
type Holder struct {
	mu  sync.Mutex
	p   Params
	d   Deps
	cur *World
	seq int
}

// NewHolder keeps p and d and copies them (plus a generated id) into every World that Ensure builds.
func NewHolder(p Params, d Deps) *Holder { return &Holder{p: p, d: d} }

// Ensure returns the current World, building and starting one if none exists; release is a no-op until M5.
// A build failure is logged and returns nil (the handler answers 503 no_world).
func (h *Holder) Ensure() (w *World, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		h.seq++
		w, err := New(h.p, Deps{ID: fmt.Sprintf("w-%d", h.seq), Clock: h.d.Clock, Logger: h.d.Logger})
		if err != nil {
			if h.d.Logger != nil {
				h.d.Logger.Error("world build failed", "err", err)
			}
			return nil, func() {}
		}
		w.Start()
		h.cur = w
	}
	return h.cur, func() {}
}

// Current returns the current World or nil; it never builds.
func (h *Holder) Current() *World {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}
