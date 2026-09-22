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

// scenarios owns the single runner goroutine; at most one scenario runs per World. cancel and done are nil while idle.
type scenarios struct {
	mu     sync.Mutex
	w      *World
	defs   map[string]*scenario
	cur    *scenario
	cancel context.CancelFunc
	done   chan struct{}
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

// StartScenario launches the named scenario's runner; ErrBusy while one runs or after the World has stopped
// (never wg.Add after Stop's Wait has begun), ErrUnknownScenario for a name the World does not define.
func (w *World) StartScenario(name string) error {
	s := w.scen
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil || w.ctx.Err() != nil {
		return ErrBusy
	}
	sc := s.defs[name]
	if sc == nil {
		return ErrUnknownScenario
	}
	ctx, cancel := context.WithCancel(w.ctx)
	d := make(chan struct{})
	s.cur, s.cancel, s.done = sc, cancel, d
	// [M4 inserts metrics.SetTargets(sc.targets()) and the timeline marker here]
	w.wg.Add(1)
	go s.run(ctx, sc, d)
	w.log.Info("scenario started", "scenario", name)
	return nil
}

// StopScenario cancels the running scenario and waits for its exit; a no-op while idle. s.mu is never held across
// the wait or any metrics call.
func (w *World) StopScenario() {
	s := w.scen
	s.mu.Lock()
	c, d := s.cancel, s.done
	s.mu.Unlock()
	if c == nil {
		return
	}
	c()
	<-d
}

// run walks the phases (each: enter, publish the card countdown, wait for its duration or a cancel), then exit
// once; the exit path clears the card and the runner slot before done is closed, so a Stop that returns sees an
// idle World. [M4 inserts the recovery tail between exit and the clear.]
func (s *scenarios) run(ctx context.Context, sc *scenario, d chan struct{}) {
	w := s.w
	defer w.wg.Done()
	defer close(d)
	cancelled := false
	for _, p := range sc.phases {
		if cancelled {
			break
		}
		p.enter()
		w.metrics.SetScenario(sc.name, p.name, w.clock.Now().Add(p.dur))
		select {
		case <-w.clock.After(p.dur):
		case <-ctx.Done():
			cancelled = true
		}
	}
	sc.exit()
	// [M4 inserts the tail here: SetCleared, poll Recovered, ClearTargets]
	w.metrics.SetScenario("", "", time.Time{})
	s.mu.Lock()
	s.cur, s.cancel, s.done = nil, nil, nil
	s.mu.Unlock()
	w.log.Info("scenario ended", "scenario", sc.name, "cancelled", cancelled)
}
