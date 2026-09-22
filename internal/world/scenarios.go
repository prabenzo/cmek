// Owner: Claude
package world

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrBusy = errors.New("world: scenario running")            // 409 {"error":"busy"}
var ErrUnknownScenario = errors.New("world: unknown scenario") // 404 {"error":"unknown_scenario"}

// phase is one timed step; enter runs at its start.
type phase struct {
	name  string
	dur   time.Duration
	enter func()
}

// scenario is a scripted fault or surge (Q19): targets is the declared affected set, defined here and first
// consumed by M4's SetTargets; phases run in order; exit restores whatever enter changed and is idempotent.
type scenario struct {
	name    string
	targets func() []int
	phases  []phase
	exit    func()
}

// scenarios owns the single runner goroutine; at most one scenario runs per World. cancel and done are nil while
// idle; stopping is set by World.Stop under mu before the World's context is cancelled, so a Start that passes the
// check has already done its wg.Add before Stop's Wait can begin.
type scenarios struct {
	mu       sync.Mutex
	w        *World
	defs     map[string]*scenario
	cur      *scenario
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
}

// stop marks the World as stopping; called by World.Stop before w.cancel().
func (s *scenarios) stop() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
}

// newScenarios defines the two surges (M3); M4 adds provider_blip, provider_outage, key_revocation and slow_kms.
func newScenarios(w *World) *scenarios {
	s := &scenarios{w: w, defs: make(map[string]*scenario)}
	p := w.P
	n := len(w.byRank)
	rank := p.SurgeTenantRank
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	surge := w.byRank[rank-1]
	s.defs["tenant_surge"] = &scenario{
		name:    "tenant_surge",
		targets: func() []int { return []int{surge} },
		phases:  []phase{{name: "surge", dur: p.TenantSurgeFor, enter: func() { w.gen.SetMultiplier(surge, p.TenantSurgeMult) }}},
		exit:    func() { w.gen.SetMultiplier(surge, 1) },
	}
	top := p.GlobalSurgeAffectedTop
	if top > n {
		top = n
	}
	s.defs["global_surge"] = &scenario{
		name:    "global_surge",
		targets: func() []int { return append([]int(nil), w.byRank[:top]...) },
		phases:  []phase{{name: "surge", dur: p.GlobalSurgeFor, enter: func() { w.gen.SetGlobal(p.GlobalSurgeMult) }}},
		exit:    func() { w.gen.SetGlobal(1) },
	}
	return s
}

// StartScenario launches the named scenario's runner; ErrBusy while one runs or once the World is stopping
// (the check and the wg.Add happen under s.mu, which Stop takes before cancelling, so never wg.Add after Stop's
// Wait has begun), ErrUnknownScenario for a name the World does not define.
func (w *World) StartScenario(name string) error {
	s := w.scen
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil || s.stopping {
		return ErrBusy
	}
	sc := s.defs[name]
	if sc == nil {
		return ErrUnknownScenario
	}
	ctx, cancel := context.WithCancel(w.ctx)
	d := make(chan struct{})
	s.cur, s.cancel, s.done = sc, cancel, d
	// the first phase is entered and its card published before the runner starts, so a caller that sees a nil
	// error also sees the effect and the card on the next snapshot; run enters the later phases
	first := sc.phases[0]
	first.enter()
	w.metrics.SetScenario(sc.name, first.name, w.clock.Now().Add(first.dur))
	// [M4 inserts metrics.SetTargets(sc.targets()) and the timeline marker here]
	w.wg.Add(1)
	go s.run(ctx, cancel, sc, d)
	w.log.Info("scenario started", "scenario", name)
	return nil
}

// StopScenario cancels the running scenario, waits for its exit and returns its name ("" while idle, a no-op).
// s.mu is never held across the wait (lock order where both are taken: s.mu, then metrics.mu).
func (w *World) StopScenario() string {
	s := w.scen
	s.mu.Lock()
	c, d, cur := s.cancel, s.done, s.cur
	s.mu.Unlock()
	if c == nil {
		return ""
	}
	c()
	<-d
	return cur.name
}

// run walks the phases (each after the first: enter, publish the card countdown; each: wait for its duration or
// a cancel), then exit once; the exit path frees the runner slot, then clears the card, before done is closed, so a Stop that returns
// sees an idle World and a poller that sees the card idle can Start. cancel is released on every path (a child context stays registered on w.ctx until it is called).
// [M4 inserts the recovery tail between exit and the clear.]
func (s *scenarios) run(ctx context.Context, cancel context.CancelFunc, sc *scenario, d chan struct{}) {
	w := s.w
	defer w.wg.Done()
	defer close(d)
	defer cancel()
	cancelled := false
	for i, p := range sc.phases {
		if cancelled {
			break
		}
		if i > 0 { // StartScenario entered the first phase
			p.enter()
			w.metrics.SetScenario(sc.name, p.name, w.clock.Now().Add(p.dur))
		}
		select {
		case <-w.clock.After(p.dur):
		case <-ctx.Done():
			cancelled = true
		}
	}
	sc.exit()
	// [M4 inserts the tail here: SetCleared, poll Recovered, ClearTargets]
	// the slot is freed before the card clears, so an observer that sees the card idle can Start again
	s.mu.Lock()
	s.cur, s.cancel, s.done = nil, nil, nil
	s.mu.Unlock()
	w.metrics.SetScenario("", "", time.Time{})
	w.log.Info("scenario ended", "scenario", sc.name, "cancelled", cancelled)
}
