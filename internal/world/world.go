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

	"github.com/prabenzo/cmek/internal/cmek"
	"github.com/prabenzo/cmek/internal/kms"
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
	rnd    *rand.Rand
	rndMu  sync.Mutex

	ids    []string
	index  map[string]int
	rankOf []int // grid index → Zipf rank (1 = heaviest)
	byRank []int // rank-1 → grid index

	kms     *kms.Fake
	keys    *cmek.Manager
	store   *queue.Store
	sched   *queue.Scheduler
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
	w := &World{ID: d.ID, P: p, ctx: ctx, cancel: cancel, clock: d.Clock, log: d.Logger.With("world", d.ID), rnd: rand.New(rand.NewSource(p.Seed)), started: d.Clock.Now()}
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
	w.keys = cmek.New(ctx, cmek.Config{Tenants: specs, Providers: p.Providers, Keys: w.kms, Store: store, Audit: auditBridge{w}, Clock: d.Clock, Jitter: jitter{w}, KMSTimeout: p.KMSTimeout, SweepInterval: p.SweepInterval})
	w.sink = traffic.NewSink(traffic.SinkConfig{Tenants: w.ids, MinLatency: p.SinkLatencyMin, MaxLatency: p.SinkLatencyMax, Ring: p.SinkRing, CanaryPrefix: p.CanaryPrefix, Clock: d.Clock, Rand: w.rnd, Lock: &w.rndMu})
	w.gen = traffic.NewGenerator(traffic.GeneratorConfig{Tenants: w.ids, Rates: rates, PayloadBytes: p.PayloadBytes, CanaryPrefix: p.CanaryPrefix, Ingest: w, Clock: d.Clock, Rand: w.rnd, Lock: &w.rndMu})
	capacity := float64(p.Workers) / ((p.SinkLatencyMin + p.SinkLatencyMax) / 2).Seconds()
	w.metrics = metrics.New(metrics.Config{Tenants: w.ids, Providers: p.Providers, Grid: &gridAdapter{w: w, buf: make([]cmek.State, n)}, Backlog: store, Offered: w.gen, Watcher: watcher{w}, Clock: d.Clock, Interval: p.SnapshotInterval, AuditRing: p.AuditRing, ViewerQueue: p.ViewerQueue, Capacity: capacity, WorldID: d.ID})
	w.sched = queue.NewScheduler(queue.SchedulerConfig{Store: store, Gate: w.keys, Tenants: w.ids})
	w.workers = queue.NewWorkers(queue.WorkersConfig{Store: store, Sched: w.sched, Keys: w.keys, Sink: w.sink, Recorder: w.metrics, Clock: d.Clock, Tenants: w.ids, Workers: p.Workers, ClaimBatch: p.ClaimBatch, ClaimTimeout: p.ClaimTimeout, IdlePoll: p.IdlePoll, Logger: w.log})
	attrs := []any{"tenants", n}
	for r := 0; r < n && r < 5; r++ { // the top ranks (the scenarios' revoke target is rank 3)
		attrs = append(attrs, fmt.Sprintf("rank%d", r+1), w.ids[w.byRank[r]])
	}
	w.log.Info("world built", append(attrs, "capacity_ps", capacity)...)
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

// Tenant returns the tenant read model (M1: state, rank, backlog, offered rate).
func (w *World) Tenant(id string) (TenantDetail, error) {
	idx, ok := w.index[id]
	if !ok {
		return TenantDetail{}, ErrUnknownTenant
	}
	states := make([]cmek.State, w.P.Tenants)
	w.keys.States(states)
	return TenantDetail{ID: id, Provider: w.P.Providers[idx*len(w.P.Providers)/w.P.Tenants], State: states[idx].String(), Rank: w.rankOf[idx], Backlog: w.store.Backlog(idx), Ready: w.store.Ready(idx), OfferedPS: w.gen.Offered(idx), Audit: []metrics.Audit{}}, nil
}
