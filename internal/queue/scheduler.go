// Owner: Claude (reviewed by Ben: Next)
package queue

import (
	"math"
	"sync"
)

// Gate says whether a tenant may be dispatched now (cmek.Manager.Hot).
type Gate interface{ Hot(tenant string) bool }

// Share tells the scheduler the per-tenant backlog above which a tenant is "over share" (admit.Gate); nil means one class.
type Share interface{ FairShare() int }

// SchedulerConfig wires the ring; TwoClass and LightTurns select the interleaved pass (M3).
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
	mu      sync.Mutex
	cursorA int // the within-share (light) class
	cursorB int // the over-share (heavy) class
	turn    int // 0..LightTurns; turns below LightTurns prefer the light class
	cfg     SchedulerConfig
}

// NewScheduler builds the ring.
func NewScheduler(cfg SchedulerConfig) *Scheduler { return &Scheduler{cfg: cfg} }

// Next returns the next dispatchable tenant with ready rows, or false after a full miss. With TwoClass the pass is
// interleaved, not prioritised (Q5): LightTurns consecutive turns prefer cursor A (the first ready tenant whose
// backlog is within the fair share and that Gate.Hot admits), then one turn prefers cursor B (the first other
// ready, hot tenant); a miss on the preferred class falls through to the other, so the ring is work-conserving.
// Hot, which may kick a renewal, runs only for a candidate of the wanted class, after the cheap ledger reads. A
// double miss leaves both cursors and the turn unchanged and the worker waits on Wake/IdlePoll. TwoClass = false
// (or FairShare = false) makes every tenant light, so cursor B never hits: the M1 ring.
func (s *Scheduler) Next() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := math.MaxInt
	if s.cfg.TwoClass && s.cfg.Share != nil {
		fs = s.cfg.Share.FairShare()
	}
	light := s.turn < s.cfg.LightTurns
	for _, want := range [2]bool{light, !light} {
		if i, ok := s.scan(want, fs); ok {
			s.turn = (s.turn + 1) % (s.cfg.LightTurns + 1)
			return i, true
		}
	}
	return 0, false
}

// scan walks at most len(Tenants) indexes from the class's cursor and accepts the first ready tenant of the wanted
// class that Gate.Hot admits, advancing the cursor past it; caller holds s.mu.
func (s *Scheduler) scan(want bool, fs int) (int, bool) {
	cur := &s.cursorA
	if !want {
		cur = &s.cursorB
	}
	n := len(s.cfg.Tenants)
	for k := 0; k < n; k++ {
		i := (*cur + k) % n
		if s.cfg.Store.Ready(i) > 0 && (s.cfg.Store.Backlog(i) <= fs) == want && s.cfg.Gate.Hot(s.cfg.Tenants[i]) {
			*cur = (i + 1) % n
			return i, true
		}
	}
	return 0, false
}
