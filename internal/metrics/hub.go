// Owner: Claude
package metrics

import "sync"

// hub fans encoded snapshots out to viewers; a slow viewer misses ticks (drop-on-slow).
type hub struct {
	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	depth   int
	closed  bool
	watcher ViewerWatcher
}

func newHub(depth int, w ViewerWatcher) *hub {
	if depth < 1 {
		depth = 1
	}
	return &hub{subs: make(map[chan []byte]struct{}), depth: depth, watcher: w}
}

func (h *hub) notify(n int) {
	if h.watcher != nil {
		h.watcher.Viewers(n)
	}
}

// subscribe adds a viewer. The watcher callback runs under hub.mu so viewer counts reach it in the order they
// happened (a cancel/subscribe pair must never deliver 0 after 1); the watcher touches only the generator gate
// and an atomic, so the only lock taken inside is gen.mu.
func (h *hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, h.depth)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	h.notify(len(h.subs))
	h.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[ch]; ok {
				delete(h.subs, ch)
				close(ch)
			}
			if !h.closed {
				h.notify(len(h.subs))
			}
		})
	}
	return ch, cancel
}

// broadcast offers b to every viewer without blocking.
func (h *hub) broadcast(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
		}
	}
}

// closeAll ends every subscription (World.Stop).
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
	h.notify(0)
}

func (h *hub) viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
