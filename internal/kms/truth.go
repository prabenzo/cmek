// Owner: Claude
package kms

import "sync"

// Truth is the ground-truth log of key events; only the checker holds it (world hands it over once), and no
// endpoint or snapshot exposes it. Key events only: no call counter [BB-11].
type Truth struct {
	mu        sync.Mutex
	keyEvents []KeyEvent
}

// KeyEvents returns a copy of every key event in order.
func (t *Truth) KeyEvents() []KeyEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]KeyEvent(nil), t.keyEvents...)
}

func (t *Truth) append(e KeyEvent) {
	t.mu.Lock()
	t.keyEvents = append(t.keyEvents, e)
	t.mu.Unlock()
}
