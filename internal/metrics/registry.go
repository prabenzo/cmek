// Owner: Claude
package metrics

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// GridSource fills the state array without blocking on the core (cmek.Manager.States via a world adapter).
type GridSource interface{ States(dst []uint8) }

// BacklogSource reads backlog for tiles and the affected-set chart (queue.Store).
type BacklogSource interface {
	Backlog(idx int) int
	Total() int
}

// OfferedSource reads the generator's current offered rate (traffic.Generator).
type OfferedSource interface{ OfferedPS() float64 }

// ViewerWatcher is told when the viewer count changes (world.World); called with no metrics lock held.
type ViewerWatcher interface{ Viewers(n int) }

// Clock is the tick's time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Audit is the metrics-side copy of a cmek audit entry (world bridges the types).
type Audit struct {
	At                  time.Time
	Idx                 int
	Op, Outcome, Detail string
	Class, From, To     uint8
	Latency             time.Duration
	Purged              int
}

// Config sizes rings and windows.
type Config struct {
	Tenants, Providers                                                                           []string
	Grid                                                                                         GridSource
	Backlog                                                                                      BacklogSource
	Offered                                                                                      OfferedSource
	Watcher                                                                                      ViewerWatcher
	Clock                                                                                        Clock
	Interval, ChartWindow                                                                        time.Duration
	P99Window, BaselineTicks, AuditRing, TimelineRing, SnapshotEvents, AggregateMin, ViewerQueue int
	Capacity                                                                                     float64
	WorldID                                                                                      string
}

// Ingest reasons, in the order the snapshot reports them.
const (
	reasonAccepted = iota
	reasonRateLimited
	reasonBacklogFull
	reasonOverloaded
	reasonKeyUnavailable
	reasonKeyRevoked
	reasonInternal
	reasonCount
)

func reasonIndex(reason string) int {
	switch reason {
	case "accepted":
		return reasonAccepted
	case "rate_limited":
		return reasonRateLimited
	case "backlog_full":
		return reasonBacklogFull
	case "overloaded":
		return reasonOverloaded
	case "key_unavailable":
		return reasonKeyUnavailable
	case "key_revoked":
		return reasonKeyRevoked
	}
	return reasonInternal
}

// Registry holds every aggregate, the audit rings and the SSE hub.
type Registry struct {
	cfg   Config
	hub   *hub
	ticks atomic.Int64

	ingest    [reasonCount]atomic.Int64 // per tick
	delivered atomic.Int64              // per tick
	kms       [3]atomic.Int64           // per tick: ok, transient, deny

	mu        sync.Mutex
	audits    [][]Audit // per tenant ring
	auditHead []int
	auditN    []int
	states    []uint8
	grid      []byte
}

// New builds the registry.
func New(cfg Config) *Registry {
	if cfg.AuditRing <= 0 {
		cfg.AuditRing = 16
	}
	n := len(cfg.Tenants)
	r := &Registry{cfg: cfg, hub: newHub(cfg.ViewerQueue, cfg.Watcher), audits: make([][]Audit, n), auditHead: make([]int, n), auditN: make([]int, n), states: make([]uint8, n), grid: make([]byte, n)}
	return r
}

// Ingest counts one outcome by reason and by share class (L4 split, M3).
func (r *Registry) Ingest(idx int, reason string, withinShare bool) {
	r.ingest[reasonIndex(reason)].Add(1)
}

// Delivered records one delivery (histograms arrive in M4).
func (r *Registry) Delivered(idx int, latency time.Duration) { r.delivered.Add(1) }

// Audit stores the entry in the tenant ring and counts KMS calls by class.
func (r *Registry) Audit(e Audit) {
	if e.Op == "unwrap" || e.Op == "generate" {
		if e.Class < 3 {
			r.kms[e.Class].Add(1)
		}
	}
	if e.Idx < 0 || e.Idx >= len(r.audits) {
		return
	}
	r.mu.Lock()
	ring := r.audits[e.Idx]
	if ring == nil {
		ring = make([]Audit, r.cfg.AuditRing)
		r.audits[e.Idx] = ring
	}
	ring[r.auditHead[e.Idx]] = e
	r.auditHead[e.Idx] = (r.auditHead[e.Idx] + 1) % r.cfg.AuditRing
	if r.auditN[e.Idx] < r.cfg.AuditRing {
		r.auditN[e.Idx]++
	}
	r.mu.Unlock()
}

// Subscribe returns a channel of encoded snapshots (cap ViewerQueue, drop-on-slow) and a cancel func.
func (r *Registry) Subscribe() (<-chan []byte, func()) { return r.hub.subscribe() }

// CloseAll ends every subscription (Stop).
func (r *Registry) CloseAll() { r.hub.closeAll() }

// Viewers is the number of open subscriptions.
func (r *Registry) Viewers() int { return r.hub.viewers() }

// Ticks is the snapshot counter (/health).
func (r *Registry) Ticks() int64 { return r.ticks.Load() }

type snapshot struct {
	World   string `json:"world"`
	Tick    int64  `json:"tick"`
	T       int64  `json:"t"`
	Viewers int    `json:"viewers"`
	Tiles   struct {
		DeliveredPS float64 `json:"delivered_ps"`
		CapacityPS  float64 `json:"capacity_ps"`
		OfferedPS   float64 `json:"offered_ps"`
		KMSCallsPS  float64 `json:"kms_calls_ps"`
		ByState     [4]int  `json:"by_state"`
	} `json:"tiles"`
	IngestPS struct {
		Accepted       float64 `json:"accepted"`
		RateLimited    float64 `json:"rate_limited"`
		BacklogFull    float64 `json:"backlog_full"`
		Overloaded     float64 `json:"overloaded"`
		KeyUnavailable float64 `json:"key_unavailable"`
		KeyRevoked     float64 `json:"key_revoked"`
		Internal       float64 `json:"internal"`
	} `json:"ingest_ps"`
	KMSPS struct {
		OK        float64 `json:"ok"`
		Transient float64 `json:"transient"`
		Deny      float64 `json:"deny"`
		EventsPS  float64 `json:"events_ps"`
	} `json:"kms_ps"`
	Backlog struct {
		Total    int `json:"total"`
		Affected int `json:"affected"`
	} `json:"backlog"`
	Grid string `json:"grid"`
}

// tick builds one snapshot: sources are read before the lock, per-tick atomics are swapped, and the encoded bytes go to the hub.
func (r *Registry) tick() {
	tick := r.ticks.Add(1)
	now := r.cfg.Clock.Now()
	perSec := float64(time.Second) / float64(r.cfg.Interval)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Grid.States(r.states)
	total := r.cfg.Backlog.Total()
	var s snapshot
	s.World, s.Tick, s.T, s.Viewers = r.cfg.WorldID, tick, now.UnixMilli(), r.hub.viewers()
	var ing [reasonCount]int64
	for i := range ing {
		ing[i] = r.ingest[i].Swap(0)
	}
	del := r.delivered.Swap(0)
	var k [3]int64
	for i := range k {
		k[i] = r.kms[i].Swap(0)
	}
	s.Tiles.DeliveredPS = float64(del) * perSec
	s.Tiles.CapacityPS = r.cfg.Capacity
	if r.cfg.Offered != nil {
		s.Tiles.OfferedPS = r.cfg.Offered.OfferedPS()
	}
	s.Tiles.KMSCallsPS = float64(k[0]+k[1]+k[2]) * perSec
	for i, st := range r.states {
		if st < 4 {
			s.Tiles.ByState[st]++
		}
		r.grid[i] = '0' + st
	}
	s.IngestPS.Accepted = float64(ing[reasonAccepted]) * perSec
	s.IngestPS.RateLimited = float64(ing[reasonRateLimited]) * perSec
	s.IngestPS.BacklogFull = float64(ing[reasonBacklogFull]) * perSec
	s.IngestPS.Overloaded = float64(ing[reasonOverloaded]) * perSec
	s.IngestPS.KeyUnavailable = float64(ing[reasonKeyUnavailable]) * perSec
	s.IngestPS.KeyRevoked = float64(ing[reasonKeyRevoked]) * perSec
	s.IngestPS.Internal = float64(ing[reasonInternal]) * perSec
	s.KMSPS.OK, s.KMSPS.Transient, s.KMSPS.Deny = float64(k[0])*perSec, float64(k[1])*perSec, float64(k[2])*perSec
	s.KMSPS.EventsPS = s.IngestPS.Accepted
	s.Backlog.Total = total
	s.Grid = string(r.grid)
	b, err := json.Marshal(&s)
	if err != nil {
		return
	}
	r.hub.broadcast(b)
}

// Run ticks every Interval until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.cfg.Clock.After(r.cfg.Interval):
			r.tick()
		}
	}
}
