// Owner: Claude (reviewed by Ben: Next)
package queue

import "sync"

// Gate says whether a tenant may be dispatched now (cmek.Manager.Hot).
type Gate interface{ Hot(tenant string) bool }

// Share tells the scheduler the per-tenant backlog above which a tenant is "over share" (admit.Gate); nil means one class.
type Share interface{ FairShare() int }

// SchedulerConfig wires the ring; TwoClass and LightTurns are M3's interleaved pass (ignored in M1).
type SchedulerConfig struct {
	Store      *Store
	Gate       Gate
	Share      Share
	Tenants    []string
	TwoClass   bool
	LightTurns int
}

// Scheduler is the fair ring; workers call Next.
type Scheduler struct {
	mu     sync.Mutex
	cursor int
	cfg    SchedulerConfig
}

// NewScheduler builds the ring.
func NewScheduler(cfg SchedulerConfig) *Scheduler { return &Scheduler{cfg: cfg} }

// Next returns the next dispatchable tenant with ready rows, one turn per tenant per pass, or false after a full miss.
func (s *Scheduler) Next() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.cfg.Tenants)
	for k := 0; k < n; k++ {
		i := (s.cursor + k) % n
		if s.cfg.Store.Ready(i) > 0 && s.cfg.Gate.Hot(s.cfg.Tenants[i]) {
			s.cursor = (i + 1) % n
			return i, true
		}
	}
	return 0, false
}
