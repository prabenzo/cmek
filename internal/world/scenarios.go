// Owner: Claude (reviewed by Ben: wiring)
package world

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrBusy = errors.New("world: scenario running")            // 409 {"error":"busy"}
var ErrUnknownScenario = errors.New("world: unknown scenario") // 404 {"error":"unknown_scenario"}

// phase is one timed step; enter runs at its start; dur 0 means open-ended: it ends on the run's signal (Restore)
// or Stop.
type phase struct {
	name  string
	dur   time.Duration
	enter func()
}

// scenario is a scripted fault or surge (Q19): targets is the declared affected set, evaluated at Start and handed
// to metrics.SetTargets (Q8); phases run in order; exit restores whatever enter changed and is idempotent; scope
// names the affected set in the restored line; marker is the timeline line posted at Start.
type scenario struct {
	name    string
	scope   string
	marker  string
	targets func() []int
	phases  []phase
	exit    func()
}

// scenarios owns the single runner goroutine; at most one scenario runs per World. cancel, done and signal are nil
// while idle; stopping is set by World.Stop under mu before the World's context is cancelled, so a Start that passes
// the check has already done its wg.Add before Stop's Wait can begin. target is the revocation run's tenant.
type scenarios struct {
	mu       sync.Mutex
	w        *World
	defs     map[string]*scenario
	cur      *scenario
	cancel   context.CancelFunc
	done     chan struct{}
	signal   chan struct{} // per run: Restore ends an open phase
	target   int
	stopping bool
}

// stop marks the World as stopping; called by World.Stop before w.cancel().
func (s *scenarios) stop() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
}

// band returns the grid indexes of one provider's contiguous third.
func (w *World) band(provider string) []int {
	out := []int{}
	for i := range w.ids {
		if w.P.Providers[i*len(w.P.Providers)/w.P.Tenants] == provider {
			out = append(out, i)
		}
	}
	return out
}

// newScenarios defines the five cards' six scenarios: the two surges (M3) and the fault scenarios (M4), mapped to
// the same flat FaultRequest, SetKey and Surge the curl API uses (ARCH Q19).
func newScenarios(w *World) *scenarios {
	s := &scenarios{w: w, defs: make(map[string]*scenario)}
	p := w.P
	n := len(w.byRank)
	rank := func(r int) int {
		if r < 1 {
			r = 1
		}
		if r > n {
			r = n
		}
		return w.byRank[r-1]
	}
	surge := rank(p.SurgeTenantRank)
	surgeID := w.ids[surge]
	s.defs["tenant_surge"] = &scenario{
		name: "tenant_surge", scope: surgeID,
		marker:  fmt.Sprintf("scenario tenant_surge started: %s ×%g for %s", surgeID, p.TenantSurgeMult, p.TenantSurgeFor),
		targets: func() []int { return []int{surge} },
		phases:  []phase{{name: "surge", dur: p.TenantSurgeFor, enter: func() { w.gen.SetMultiplier(surge, p.TenantSurgeMult) }}},
		exit:    func() { w.gen.SetMultiplier(surge, 1) },
	}
	top := p.GlobalSurgeAffectedTop
	if top > n {
		top = n
	}
	s.defs["global_surge"] = &scenario{
		name: "global_surge", scope: fmt.Sprintf("top %d", top),
		marker:  fmt.Sprintf("scenario global_surge started: every tenant ×%g for %s", p.GlobalSurgeMult, p.GlobalSurgeFor),
		targets: func() []int { return append([]int(nil), w.byRank[:top]...) },
		phases:  []phase{{name: "surge", dur: p.GlobalSurgeFor, enter: func() { w.gen.SetGlobal(p.GlobalSurgeMult) }}},
		exit:    func() { w.gen.SetGlobal(1) },
	}
	fault := func(f FaultRequest) func() {
		return func() {
			if err := w.Fault(f); err != nil {
				w.log.Error("scenario fault", "err", err)
			}
		}
	}
	out := p.OutageProvider
	for _, o := range []struct {
		name string
		dur  time.Duration
	}{{"provider_blip", p.OutageBlip}, {"provider_outage", p.OutageLong}} {
		s.defs[o.name] = &scenario{
			name: o.name, scope: out,
			marker:  fmt.Sprintf("scenario %s started: %s fast-fail for %s", o.name, out, o.dur),
			targets: func() []int { return w.band(out) },
			phases:  []phase{{name: "fault", dur: o.dur, enter: fault(FaultRequest{Provider: out, Mode: "fast_fail"})}},
			exit:    fault(FaultRequest{Provider: out, Mode: "ok"}),
		}
	}
	slow := p.SlowProvider
	s.defs["slow_kms"] = &scenario{
		name: "slow_kms", scope: slow,
		marker:  fmt.Sprintf("scenario slow_kms started: %s p50 %s / p99 %s for %s", slow, p.SlowP50, p.SlowP99, p.SlowFor),
		targets: func() []int { return w.band(slow) },
		phases:  []phase{{name: "slow", dur: p.SlowFor, enter: fault(FaultRequest{Provider: slow, LatencyP50Ms: int(p.SlowP50 / time.Millisecond), LatencyP99Ms: int(p.SlowP99 / time.Millisecond)})}},
		exit:    fault(FaultRequest{Provider: slow, Mode: "ok"}),
	}
	rev := rank(p.RevokeTenantRank)
	revID := w.ids[rev]
	s.defs["key_revocation"] = &scenario{
		name: "key_revocation", scope: revID,
		marker:  fmt.Sprintf("scenario key_revocation started: %s KEK disabled (rank %d); Restore ends it", revID, w.rankOf[rev]),
		targets: func() []int { return []int{rev} },
		phases:  []phase{{name: "revoked", dur: 0, enter: func() { w.kms.Revoke(w.specs[rev].KEKID) }}},
		exit:    func() { w.kms.Restore(w.specs[rev].KEKID) },
	}
	return s
}

// StartScenario launches the named scenario's runner; ErrBusy while one runs or once the World is stopping
// (the check and the wg.Add happen under s.mu, which Stop takes before cancelling, so never wg.Add after Stop's
// Wait has begun), ErrUnknownScenario for a name the World does not define. Order: targets (freezes the baseline),
// the first phase's enter, its card, the marker, then the runner, so a caller that gets nil sees all of it on the
// next snapshot.
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
	s.cur, s.cancel, s.done, s.signal = sc, cancel, d, make(chan struct{}, 1)
	s.target = -1
	if t := sc.targets(); name == "key_revocation" && len(t) == 1 {
		s.target = t[0]
	}
	w.metrics.SetTargets(sc.targets())
	first := sc.phases[0]
	first.enter()
	var ends time.Time
	if first.dur > 0 {
		ends = w.clock.Now().Add(first.dur)
	}
	w.metrics.SetScenario(sc.name, first.name, ends)
	w.metrics.Timeline(sc.marker)
	w.wg.Add(1)
	go s.run(ctx, cancel, sc, d, s.signal)
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

// restore ends the running key_revocation run's open phase when idx is its target (a non-blocking send on the
// per-run channel: a stale token can never end the next run); any other restore is the plain endpoint.
func (s *scenarios) restore(idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.name != "key_revocation" || s.target != idx {
		return
	}
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// run walks the phases (each after the first: enter, publish the card countdown; each: wait for its duration, the
// run's signal for an open phase, or a cancel), then exit once. A normal end runs the tail (recovery and drain
// times, the restored line); a cancel skips it. The exit path frees the runner slot, then clears the targets and
// the card, before done is closed, so a Stop that returns sees an idle World and a poller that sees the card idle
// can Start. cancel is released on every path (a child context stays registered on w.ctx until it is called).
func (s *scenarios) run(ctx context.Context, cancel context.CancelFunc, sc *scenario, d chan struct{}, sig chan struct{}) {
	w := s.w
	defer w.wg.Done()
	defer close(d)
	defer cancel()
	cancelled := false
	for i, p := range sc.phases {
		if cancelled {
			break
		}
		var t <-chan time.Time
		var open <-chan struct{}
		if i > 0 { // StartScenario entered the first phase
			p.enter()
			var ends time.Time
			if p.dur > 0 {
				ends = w.clock.Now().Add(p.dur)
			}
			w.metrics.SetScenario(sc.name, p.name, ends)
		}
		if p.dur > 0 {
			t = w.clock.After(p.dur)
		} else {
			open = sig // a nil channel blocks: a timed phase ignores the signal, an open one never fires at once
		}
		select {
		case <-t:
		case <-open:
		case <-ctx.Done():
			cancelled = true
		}
	}
	sc.exit()
	if !cancelled {
		s.tail(ctx, sc)
	}
	s.mu.Lock()
	s.cur, s.cancel, s.done, s.signal = nil, nil, nil, nil
	s.mu.Unlock()
	w.metrics.ClearTargets()
	w.metrics.SetScenario("", "", time.Time{})
	w.log.Info("scenario ended", "scenario", sc.name, "cancelled", cancelled)
}

// tail runs after a normal end: SetCleared(now); every SnapshotInterval poll metrics.Recovered(); end at
// ≥ ScenarioTailMin once done, at ScenarioTailMax, or on a cancel (no restored line then); post "<scope> restored:
// N tenants ACTIVE in x s, backlog drained in y s". The card shows the recovery phase with the minimum tail as its
// countdown, and the snapshot carries recovery_s / drain_s as they fill.
func (s *scenarios) tail(ctx context.Context, sc *scenario) {
	w := s.w
	start := w.clock.Now()
	w.metrics.SetCleared(start)
	w.metrics.SetScenario(sc.name, "recovery", start.Add(w.P.ScenarioTailMin))
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(w.P.SnapshotInterval):
		}
		rec, drain, done := w.metrics.Recovered()
		el := w.clock.Now().Sub(start)
		if (done && el >= w.P.ScenarioTailMin) || el >= w.P.ScenarioTailMax {
			n := len(sc.targets())
			switch {
			case done:
				w.metrics.Timeline(fmt.Sprintf("%s restored: %d tenants ACTIVE in %.1f s, backlog drained in %.1f s", sc.scope, n, rec.Seconds(), drain.Seconds()))
			case rec > 0:
				w.metrics.Timeline(fmt.Sprintf("%s restored: %d tenants ACTIVE in %.1f s, backlog not drained within %s", sc.scope, n, rec.Seconds(), w.P.ScenarioTailMax))
			default:
				w.metrics.Timeline(fmt.Sprintf("%s not restored: targets still degraded after %s", sc.scope, w.P.ScenarioTailMax))
			}
			return
		}
	}
}
