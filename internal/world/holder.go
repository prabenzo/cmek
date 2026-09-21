// Owner: Claude (reviewed by Ben)
package world

import (
	"fmt"
	"sync"
	"time"
)

// Holder owns "the current World" for main; instantiated once in main, never package-level.
// M0 ships Ensure and Current only; M5 adds Acquire's idle rule, Reset and the handler refcount.
type Holder struct {
	mu  sync.Mutex
	p   Params
	cur *World
	seq int
}

// NewHolder builds an empty holder; the first Ensure builds "w-1".
func NewHolder(p Params) *Holder { return &Holder{p: p} }

// Ensure returns the current World, building one if none exists; release is a no-op until M5.
func (h *Holder) Ensure() (w *World, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		h.seq++
		h.cur = &World{ID: fmt.Sprintf("w-%d", h.seq), P: h.p, started: time.Now()}
	}
	return h.cur, func() {}
}

// Current returns the current World or nil; it never builds.
func (h *Holder) Current() *World {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}
