// Owner: Claude (reviewed by Ben: wiring)
package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prabenzo/cmek/internal/admit"
	"github.com/prabenzo/cmek/internal/cmek"
	"github.com/prabenzo/cmek/internal/kms"
	"github.com/prabenzo/cmek/internal/logx"
	"github.com/prabenzo/cmek/internal/metrics"
	"github.com/prabenzo/cmek/internal/queue"
	"github.com/prabenzo/cmek/internal/traffic"
)

// Clock is satisfied by the real clock and by a test clock (settable Now, After delegating to time.After).
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Deps are the injectables a test replaces; ID is the World id ("w-<n>" from the Holder, any unique string in tests). Zero Clock means the real clock.
type Deps struct {
	ID     string
	Clock  Clock
	Logger *slog.Logger
}

// World owns one complete running system: service, fakes, checker, metrics. No globals anywhere.
type World struct {
	ID string
	P  Params

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	clock  Clock
	log    *slog.Logger
	errLog logx.Throttle // internal ingest errors, one line per second per message
	rnd    *rand.Rand
	rndMu  sync.Mutex

	ids    []string
	index  map[string]int
	specs  []cmek.TenantSpec // per-tenant wiring incl. the KEK id; Fault and SetKey read it rather than rebuild it
	rankOf []int             // grid index → Zipf rank (1 = heaviest)
	byRank []int             // rank-1 → grid index

	kms     *kms.Fake
	keys    *cmek.Manager
	admit   *admit.Gate
	store   *queue.Store
	sched   *queue.Scheduler
	scen    *scenarios
	workers *queue.Workers
	gen     *traffic.Generator
	sink    *traffic.Sink
	metrics *metrics.Registry

	started   time.Time
	idleSince atomic.Int64 // unix ms when viewers dropped to 0; 0 while watched
}

// HealthInfo is the /health body.
type HealthInfo struct {
	World        string  `json:"world"`
	Ticks        int64   `json:"ticks"`
	UptimeS      float64 `json:"uptime_s"`
	Viewers      int     `json:"viewers"`
	InsertMeanUs int64   `json:"insert_mean_us"` // M1 load instrument
	InsertMaxUs  int64   `json:"insert_max_us"`
	CensusMaxUs  int64   `json:"census_max_us"`
}

// New builds a stopped World: opens SQLite, builds the fakes, shuffles ranks, wires every package.
func New(p Params, d Deps) (*World, error) {
	if d.ID == "" {
		return nil, errors.New("world: empty id")
	}
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &World{ID: d.ID, P: p, ctx: ctx, cancel: cancel, clock: d.Clock, log: d.Logger.With("world", d.ID), errLog: logx.Throttle{Log: d.Logger.With("world", d.ID)}, rnd: rand.New(rand.NewSource(p.Seed)), started: d.Clock.Now()}
	n := p.Tenants
	w.ids = make([]string, n)
	w.index = make(map[string]int, n)
	specs := make([]cmek.TenantSpec, n)
	keks := make([]kms.KEKSpec, n)
	for i := range w.ids {
		id := fmt.Sprintf("t-%04d", i)
		prov := p.Providers[i*len(p.Providers)/n]
		w.ids[i], w.index[id] = id, i
		specs[i] = cmek.TenantSpec{ID: id, Provider: prov, KEKID: "kek-" + id}
		keks[i] = kms.KEKSpec{ID: "kek-" + id, Provider: prov, Idx: i}
	}
	w.specs = specs
	w.byRank = w.rnd.Perm(n) // rank r (1-based) lives at grid index byRank[r-1]
	w.rankOf = make([]int, n)
	for r, idx := range w.byRank {
		w.rankOf[idx] = r + 1
	}
	zipf := traffic.ZipfRates(n, p.BaseRate, p.ZipfExponent)
	rates := make([]float64, n)
	for r, idx := range w.byRank {
		rates[idx] = zipf[r]
	}
	w.kms = kms.NewFake(kms.FakeConfig{Providers: p.Providers, KEKs: keks, Clock: d.Clock, Rand: w.rnd, Lock: &w.rndMu})
	dir := p.DBDir
	if dir == "" {
		dir = os.TempDir()
	}
	store, err := queue.Open(queue.Config{Path: filepath.Join(dir, fmt.Sprintf("killswitch-%d-%s.db", os.Getpid(), d.ID)), Tenants: w.ids, Clock: d.Clock, SyncMode: p.SyncMode, WALAutocheckpoint: p.WALAutocheckpoint, Logger: w.log})
	if err != nil {
		cancel()
		return nil, err
	}
	w.store = store
	w.keys = cmek.New(ctx, cmek.Config{
		Tenants: specs, Providers: p.Providers, Keys: w.kms, Store: store, Audit: auditBridge{w}, Clock: d.Clock, Jitter: jitter{w}, Logger: w.log,
		Lease: p.Lease, SoftTTL: p.SoftTTL, EarlyExpiry: p.EarlyExpiry, KMSTimeout: p.KMSTimeout, BackoffMin: p.BackoffMin, BackoffMax: p.BackoffMax,
		BackoffJitter: p.BackoffJitter, RevokedReprobe: p.RevokedReprobe, DEKMaxAge: p.DEKMaxAge, DEKMaxMessages: p.DEKMaxMessages,
		ProviderInflight: p.ProviderInflight, TenantInflight: p.TenantInflight, IngestWaiters: p.IngestWaiters, SweepInterval: p.SweepInterval,
	})
	w.sink = traffic.NewSink(traffic.SinkConfig{Tenants: w.ids, MinLatency: p.SinkLatencyMin, MaxLatency: p.SinkLatencyMax, Ring: p.SinkRing, CanaryPrefix: p.CanaryPrefix, Clock: d.Clock, Rand: w.rnd, Lock: &w.rndMu})
	w.gen = traffic.NewGenerator(traffic.GeneratorConfig{Tenants: w.ids, Rates: rates, PayloadBytes: p.PayloadBytes, CanaryPrefix: p.CanaryPrefix, Ingest: w, Clock: d.Clock, Rand: w.rnd, Lock: &w.rndMu})
	capacity := float64(p.Workers) / ((p.SinkLatencyMin + p.SinkLatencyMax) / 2).Seconds()
	w.metrics = metrics.New(metrics.Config{Tenants: w.ids, Providers: p.Providers, Grid: &gridAdapter{w: w, buf: make([]cmek.State, n)}, Backlog: store, Offered: w.gen, Inflight: w.keys, Watcher: watcher{w}, Clock: d.Clock,
		Interval: p.SnapshotInterval, ChartWindow: p.ChartWindow, P99Window: p.P99Window, BaselineTicks: p.BaselineTicks, AggregateMin: p.TimelineAggregateMin, DrainSlack: p.Workers * p.ClaimBatch,
		AuditRing: p.AuditRing, TimelineRing: p.TimelineRing, SnapshotEvents: p.SnapshotEvents, ViewerQueue: p.ViewerQueue, Capacity: capacity, WorldID: d.ID})
	w.admit = admit.New(admit.Config{Tenants: n, Rate: p.TenantRate, Burst: p.TenantBurst, TenantCap: p.TenantBacklogCap, GlobalCap: p.GlobalBacklogCap, FairShare: p.FairShare, Backlog: store, Clock: d.Clock})
	w.sched = queue.NewScheduler(queue.SchedulerConfig{Store: store, Gate: w.keys, Share: w.admit, Tenants: w.ids, TwoClass: p.TwoClassSched, LightTurns: p.SchedLightTurns})
	w.scen = newScenarios(w)
	w.workers = queue.NewWorkers(queue.WorkersConfig{Store: store, Sched: w.sched, Keys: w.keys, Sink: w.sink, Recorder: w.metrics, Clock: d.Clock, Tenants: w.ids, Workers: p.Workers, ClaimBatch: p.ClaimBatch, ClaimTimeout: p.ClaimTimeout, IdlePoll: p.IdlePoll, Logger: w.log})
	attrs := []any{"tenants", n}
	for r := 0; r < n && r < 5; r++ { // the top ranks (the scenarios' revoke target is rank 3)
		attrs = append(attrs, fmt.Sprintf("rank%d", r+1), w.ids[w.byRank[r]])
	}
	w.log.Info("world built", append(attrs, "capacity_ps", capacity)...)
	ranks := []any{}
	for _, r := range []int{1, 2, 3, 5, 20, 30, 120, 150, 200, 500, 900} { // the rank → id map the acceptance curls read
		if r <= n {
			ranks = append(ranks, fmt.Sprintf("rank%d", r), w.ids[w.byRank[r-1]])
		}
	}
	w.log.Info("ranks", ranks...)
	return w, nil
}

func (w *World) spawn(f func(context.Context)) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		f(w.ctx)
	}()
}

// Start launches every goroutine in the inventory. Traffic starts paused until the first viewer.
func (w *World) Start() {
	w.spawn(w.workers.Run)
	w.spawn(w.gen.Run)
	w.spawn(w.metrics.Run)
	w.spawn(w.sweep)
}

// sweep drives the core's Tick every SweepInterval and the queue's Reclaim every ReclaimInterval.
func (w *World) sweep(ctx context.Context) {
	lastReclaim := w.clock.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(w.P.SweepInterval):
			now := w.clock.Now()
			w.keys.Tick(now)
			if now.Sub(lastReclaim) >= w.P.ReclaimInterval {
				lastReclaim = now
				if _, err := w.store.Reclaim(ctx, now); err != nil && ctx.Err() == nil {
					w.log.Error("reclaim", "err", err)
				}
			}
		}
	}
}

// Stop cancels the context, closes every subscriber channel, waits ≤ StopTimeout for goroutines, then closes and deletes the DB file; a late goroutine is logged, not waited for.
func (w *World) Stop() {
	w.scen.stop() // no scenario may start (and wg.Add) once the wait below can begin
	w.cancel()
	w.metrics.CloseAll()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-w.clock.After(w.P.StopTimeout):
		w.log.Error("stop: goroutines still running after StopTimeout")
	}
	if err := w.store.Close(); err != nil {
		w.log.Error("store close", "err", err)
	}
}

// Subscribe hands the metrics hub's channel to /v1/stream (M5's Holder.Acquire wraps it).
func (w *World) Subscribe() (<-chan []byte, func()) { return w.metrics.Subscribe() }

// Viewers is the number of open SSE subscriptions.
func (w *World) Viewers() int { return w.metrics.Viewers() }

// IdleFor is how long there have been no viewers (0 while watched).
func (w *World) IdleFor(now time.Time) time.Duration {
	since := w.idleSince.Load()
	if since == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(since))
}

// Health is the /health body.
func (w *World) Health() HealthInfo {
	st := w.store.Stats()
	return HealthInfo{World: w.ID, Ticks: w.metrics.Ticks(), UptimeS: w.clock.Now().Sub(w.started).Seconds(), Viewers: w.Viewers(), InsertMeanUs: st.MeanUs, InsertMaxUs: st.MaxUs, CensusMaxUs: st.CensusMaxUs}
}

// watcher implements metrics.ViewerWatcher: 0 viewers pauses traffic and stamps idleSince; the first viewer resumes it.
type watcher struct{ w *World }

func (v watcher) Viewers(n int) {
	if n == 0 {
		v.w.idleSince.Store(v.w.clock.Now().UnixMilli())
		v.w.gen.SetRunning(false)
		return
	}
	v.w.gen.SetRunning(true)
	v.w.idleSince.Store(0)
}

// gridAdapter adapts cmek.Manager.States to metrics.GridSource; buf is reused by the single metrics tick goroutine.
type gridAdapter struct {
	w   *World
	buf []cmek.State
}

func (g *gridAdapter) States(dst []uint8) {
	g.w.keys.States(g.buf)
	for i, s := range g.buf[:len(dst)] {
		dst[i] = uint8(s)
	}
}

// auditBridge converts cmek.Audit to metrics.Audit (the bridge that keeps metrics from importing cmek).
type auditBridge struct{ w *World }

func (b auditBridge) Audit(e cmek.Audit) {
	idx, ok := b.w.index[e.Tenant]
	if !ok {
		idx = -1
	}
	b.w.metrics.Audit(metrics.Audit{At: e.At, Idx: idx, Op: e.Op, Outcome: e.Outcome, Detail: e.Detail, Class: uint8(e.Class), From: uint8(e.From), To: uint8(e.To), Latency: e.Latency, Purged: e.Purged})
}

// jitter is the core's only randomness (backoff), from the World's seeded rand.
type jitter struct{ w *World }

func (j jitter) Float64() float64 {
	j.w.rndMu.Lock()
	defer j.w.rndMu.Unlock()
	return j.w.rnd.Float64()
}

// TenantDetail is the body of GET /v1/tenants/{id}; M1 fills the fields the data path knows, M4 the rest.
type TenantDetail struct {
	ID               string          `json:"id"`
	Provider         string          `json:"provider"`
	State            string          `json:"state"`
	Rank             int             `json:"rank"`
	LeaseAgeMs       int64           `json:"lease_age_ms"`
	LeaseRemainingMs int64           `json:"lease_remaining_ms"`
	NextProbeMs      int64           `json:"next_probe_ms"`
	Backlog          int             `json:"backlog"`
	Ready            int             `json:"ready"`
	OfferedPS        float64         `json:"offered_ps"`
	KMSCallsPerMin   float64         `json:"kms_calls_per_min"`
	Affected         bool            `json:"affected"`
	Audit            []metrics.Audit `json:"audit"`
}

// Tenant returns the tenant read model: state and lease (cmek), rank, backlog, offered rate, KMS calls per minute
// and the last 10 audit entries (metrics ring), and the affected bit (hover and pin read it, Q11).
func (w *World) Tenant(id string) (TenantDetail, error) {
	idx, ok := w.index[id]
	if !ok {
		return TenantDetail{}, ErrUnknownTenant
	}
	info := w.keys.Info(id)
	return TenantDetail{ID: id, Provider: w.P.Providers[idx*len(w.P.Providers)/w.P.Tenants], State: info.State.String(), Rank: w.rankOf[idx],
		LeaseAgeMs: info.LeaseAge.Milliseconds(), LeaseRemainingMs: info.LeaseRemaining.Milliseconds(), NextProbeMs: info.NextProbeIn.Milliseconds(),
		Backlog: w.store.Backlog(idx), Ready: w.store.Ready(idx), OfferedPS: w.gen.Offered(idx),
		KMSCallsPerMin: w.metrics.TenantCallsPerMin(idx), Affected: w.metrics.Affected(idx), Audit: w.metrics.TenantAudit(idx, 10)}, nil
}

// FaultRequest is the flat body of POST /v1/faults: one of Provider or Tenant, plus a mode and optional latency
// and error rate (zero fields inherit). Manual faults never touch the affected set; only scenarios do (M3).
type FaultRequest struct {
	Provider     string  `json:"provider,omitempty"`
	Tenant       string  `json:"tenant,omitempty"`
	Mode         string  `json:"mode,omitempty"` // "", "ok" or "fast_fail"
	LatencyP50Ms int     `json:"latency_p50_ms,omitempty"`
	LatencyP99Ms int     `json:"latency_p99_ms,omitempty"`
	ErrorRate    float64 `json:"error_rate,omitempty"`
}

// ErrBadFault is a fault request the World refuses (400).
var ErrBadFault = errors.New("world: bad fault request")

// Fault validates the request and installs it in the fake KMS; a body with mode "ok" and no latency clears the scope.
func (w *World) Fault(f FaultRequest) error {
	var scope kms.Scope
	switch {
	case f.Provider != "" && f.Tenant != "":
		return fmt.Errorf("%w: provider or tenant, not both", ErrBadFault)
	case f.Provider != "":
		known := false
		for _, p := range w.P.Providers {
			known = known || p == f.Provider
		}
		if !known {
			return fmt.Errorf("%w: unknown provider %q", ErrBadFault, f.Provider)
		}
		scope.Provider = f.Provider
	case f.Tenant != "":
		idx, ok := w.index[f.Tenant]
		if !ok {
			return ErrUnknownTenant
		}
		scope.KEKID = w.specs[idx].KEKID
	default:
		return fmt.Errorf("%w: provider or tenant required", ErrBadFault)
	}
	var fault kms.Fault
	switch f.Mode {
	case "", "ok":
	case "fast_fail":
		fault.Mode = kms.ModeFastFail
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrBadFault, f.Mode)
	}
	// Latency: the fake gates on p50 and draws σ from p99 ≥ p50, so a p50-only body means "no spread" and a
	// p99-only body would be a silent no-op, which is refused rather than installed.
	if f.LatencyP50Ms < 0 || f.LatencyP99Ms < 0 || f.ErrorRate < 0 || f.ErrorRate > 1 {
		return fmt.Errorf("%w: latencies ≥ 0 and 0 ≤ error_rate ≤ 1", ErrBadFault)
	}
	if f.LatencyP99Ms > 0 && f.LatencyP50Ms == 0 {
		return fmt.Errorf("%w: latency_p99_ms needs latency_p50_ms", ErrBadFault)
	}
	if f.LatencyP50Ms > 0 && f.LatencyP99Ms == 0 {
		f.LatencyP99Ms = f.LatencyP50Ms
	}
	if f.LatencyP99Ms < f.LatencyP50Ms {
		return fmt.Errorf("%w: latency_p99_ms < latency_p50_ms", ErrBadFault)
	}
	fault.P50, fault.P99, fault.ErrorRate = time.Duration(f.LatencyP50Ms)*time.Millisecond, time.Duration(f.LatencyP99Ms)*time.Millisecond, f.ErrorRate
	w.kms.SetFault(scope, fault)
	w.log.Info("fault", "provider", f.Provider, "tenant", f.Tenant, "mode", f.Mode, "p50_ms", f.LatencyP50Ms, "p99_ms", f.LatencyP99Ms, "error_rate", f.ErrorRate)
	return nil
}

// Surge multiplies one tenant's offered rate, or everyone's when tenant is ""; the handler rejects mult ≤ 0.
func (w *World) Surge(tenant string, mult float64) error {
	if tenant == "" {
		w.gen.SetGlobal(mult)
		w.log.Info("surge", "tenant", "", "multiplier", mult)
		return nil
	}
	idx, ok := w.index[tenant]
	if !ok {
		return ErrUnknownTenant
	}
	w.gen.SetMultiplier(idx, mult)
	w.log.Info("surge", "tenant", tenant, "multiplier", mult)
	return nil
}

// SetKey maps "revoke" / "restore" to the fake KMS's Revoke / Restore of the tenant's KEK; a restore also signals
// the running key_revocation scenario when the tenant is its target.
func (w *World) SetKey(tenant, action string) error {
	idx, ok := w.index[tenant]
	if !ok {
		return ErrUnknownTenant
	}
	switch action {
	case "revoke":
		w.kms.Revoke(w.specs[idx].KEKID)
	case "restore":
		w.kms.Restore(w.specs[idx].KEKID)
		w.scen.restore(idx) // ends a running key_revocation on this tenant (a no-op otherwise)
	default:
		return fmt.Errorf("%w: unknown action %q", ErrBadFault, action)
	}
	w.log.Info("key", "tenant", tenant, "action", action)
	return nil
}
