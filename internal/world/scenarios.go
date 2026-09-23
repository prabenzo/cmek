// Owner: Claude (reviewed by Ben: wiring)
package world

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prabenzo/cmek/internal/metrics"
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
// to metrics.SetTargets (Q8); phases run in order; exit restores whatever enter changed and is idempotent; summary,
// when set, posts the run's numbers and runs only after a natural end (a stopped run has no finding to report);
// scope names the affected set in the restored line; marker is the timeline line posted at Start.
type scenario struct {
	name    string
	scope   string
	marker  string
	targets func() []int
	phases  []phase
	exit    func()
	summary func()
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

// newScenarios defines the six cards' eight scenarios: the two surges (M3), the fault scenarios (M4) and the two
// no-cache runs (NOCACHE.md), mapped to the same flat FaultRequest, SetKey, Surge and SetCache the curl API uses
// (ARCH Q19).
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
	// No key cache (NOCACHE.md): two runs at a realistic KMS latency on every provider, so the cache is the one variable.
	latencyAll := func(on bool) {
		for _, prov := range p.Providers {
			f := FaultRequest{Provider: prov}
			if on {
				f.LatencyP50Ms, f.LatencyP99Ms = int(p.NoCacheKMSP50/time.Millisecond), int(p.NoCacheKMSP99/time.Millisecond)
			}
			fault(f)()
		}
	}
	withLatency := func(prov, mode string) func() {
		return fault(FaultRequest{Provider: prov, Mode: mode, LatencyP50Ms: int(p.NoCacheKMSP50 / time.Millisecond), LatencyP99Ms: int(p.NoCacheKMSP99 / time.Millisecond)})
	}
	cacheProv := p.NoCacheProvider
	other := p.Providers[0]
	if other == cacheProv && len(p.Providers) > 1 {
		other = p.Providers[1]
	}
	var bandCalls, otherCalls, bandP99, healthyP99 float64
	var parked int
	s.defs["no_cache"] = &scenario{
		name: "no_cache", scope: cacheProv,
		marker:  fmt.Sprintf("scenario no_cache started: %s tenants run without the key cache for %s; every event is two KMS calls; all providers at p50 %s", cacheProv, p.NoCacheFor+p.NoCacheBlip+p.NoCacheAfter, p.NoCacheKMSP50),
		targets: func() []int { return w.band(cacheProv) },
		phases: []phase{
			{name: "cache_off", dur: p.NoCacheFor, enter: func() {
				bandCalls, otherCalls, bandP99, healthyP99, parked = 0, 0, 0, 0, 0
				latencyAll(true)
				w.SetCache(cacheProv, false)
				w.metrics.ResetPeaks()
			}},
			{name: "blip", dur: p.NoCacheBlip, enter: func() {
				pk := w.metrics.Peaks()
				bandCalls, otherCalls, bandP99, healthyP99 = pk.ProviderCallsPS[cacheProv], pk.ProviderCallsPS[other], pk.AffectedP99Ms, pk.HealthyP99Ms
				w.metrics.ResetPeaks()
				withLatency(cacheProv, "fast_fail")()
			}},
			{name: "cache_off", dur: p.NoCacheAfter, enter: func() {
				parked = w.metrics.Peaks().ByState[2]
				withLatency(cacheProv, "ok")()
				w.metrics.Timeline(fmt.Sprintf("no cache: the %s blip parked %d tenants KEY_UNAVAILABLE, each on its first event (with the cache: a yellow ride-through, no 503)", cacheProv, parked))
			}},
		},
		exit: func() {
			w.SetCache(cacheProv, true)
			latencyAll(false)
		},
		summary: func() {
			w.metrics.Timeline(fmt.Sprintf("no cache (%s, %s): KMS calls peaked at %s %.0f/s vs %s %.0f/s · p99 peaked at %.0f ms affected vs %.0f ms healthy · blip: %d tenants KEY_UNAVAILABLE",
				cacheProv, p.NoCacheFor+p.NoCacheBlip+p.NoCacheAfter, cacheProv, bandCalls, other, otherCalls, bandP99, healthyP99, parked))
		},
	}
	capacity := float64(p.Workers) / ((p.SinkLatencyMin + p.SinkLatencyMax) / 2).Seconds()
	var lead, surged metricsReading
	s.defs["no_cache_surge"] = &scenario{
		name: "no_cache_surge", scope: fmt.Sprintf("top %d", top),
		marker:  fmt.Sprintf("scenario no_cache_surge started: every tenant runs without the key cache for %s at p50 %s; the global surge (×%g) starts at %s", p.NoCacheSurgeLead+p.NoCacheSurgeFor+p.NoCacheSurgeAfter, p.NoCacheKMSP50, p.GlobalSurgeMult, p.NoCacheSurgeLead),
		targets: func() []int { return append([]int(nil), w.byRank[:top]...) },
		phases: []phase{
			{name: "cache_off", dur: p.NoCacheSurgeLead, enter: func() {
				lead, surged = metricsReading{}, metricsReading{}
				latencyAll(true)
				w.SetCache("", false)
				w.metrics.ResetPeaks()
			}},
			{name: "surge", dur: p.NoCacheSurgeFor, enter: func() {
				lead = metricsReading(w.metrics.Last())
				w.metrics.Timeline(fmt.Sprintf("no cache (everyone): delivered %.0f/s at %.0f/s accepted (capacity %.0f/s with the cache) · KMS calls %.0f/s · backlog %d and climbing", lead.DeliveredPS, lead.AcceptedPS, capacity, lead.KMSCallsPS, lead.Backlog))
				w.metrics.ResetPeaks()
				w.gen.SetGlobal(p.GlobalSurgeMult)
			}},
			{name: "cache_off", dur: p.NoCacheSurgeAfter, enter: func() {
				surged = metricsReading(w.metrics.Peaks())
				w.gen.SetGlobal(1)
				w.metrics.Timeline(fmt.Sprintf("no cache + ×%g surge, peaks: KMS calls %.0f/s · in flight %s · backlog %d · end-to-end p99 %.0f s · delivered never above %.0f/s (with the cache: delivered ≈ %.0f/s, KMS ≤ 70/s, backlog under its cap)",
					p.GlobalSurgeMult, surged.KMSCallsPS, inflightText(p.Providers, surged.Inflight), surged.Backlog, surged.AffectedP99Ms/1000, surged.DeliveredPS, capacity))
			}},
		},
		exit: func() {
			w.gen.SetGlobal(1)
			w.SetCache("", true)
			latencyAll(false)
		},
		summary: func() {
			w.metrics.Timeline(fmt.Sprintf("no cache (everyone, %s): delivered %.0f/s before the surge at %.0f/s accepted, never above %.0f/s during it (capacity %.0f/s) · KMS calls %.0f/s, peak %.0f/s",
				p.NoCacheSurgeLead+p.NoCacheSurgeFor+p.NoCacheSurgeAfter, lead.DeliveredPS, lead.AcceptedPS, surged.DeliveredPS, capacity, lead.KMSCallsPS, surged.KMSCallsPS))
		},
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

// metricsReading is the metrics package's last-tick reading, named here so the scenario closures can hold one.
type metricsReading = metrics.Reading

// inflightText renders the in-flight map in provider order: "aws 29 gcp 31 azure 30".
func inflightText(providers []string, inflight map[string]int) string {
	out := ""
	for i, p := range providers {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s %d", p, inflight[p])
	}
	return out
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
		if sc.summary != nil {
			sc.summary()
		}
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
// countdown and, once the minimum has passed with the drain still pending, as open-ended (ends_at null: the page
// says "waiting for drain"); the snapshot carries recovery_s / drain_s as they fill.
func (s *scenarios) tail(ctx context.Context, sc *scenario) {
	w := s.w
	start := w.clock.Now()
	w.metrics.SetCleared(start)
	w.metrics.SetScenario(sc.name, "recovery", start.Add(w.P.ScenarioTailMin))
	openEnded := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(w.P.SnapshotInterval):
		}
		rec, drain, done := w.metrics.Recovered()
		el := w.clock.Now().Sub(start)
		if !openEnded && el >= w.P.ScenarioTailMin {
			openEnded = true
			w.metrics.SetScenario(sc.name, "recovery", time.Time{})
		}
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
