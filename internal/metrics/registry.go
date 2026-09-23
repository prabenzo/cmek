// Owner: Claude
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// InflightSource reports KMS calls inside a provider (cmek.Manager.Inflight); read in tick step 2 before reg.mu,
// it feeds the snapshot inflight{} and the #inflight tile.
type InflightSource interface{ Inflight(provider string) int }

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
	Inflight                                                                                     InflightSource
	Watcher                                                                                      ViewerWatcher
	Clock                                                                                        Clock
	Interval, ChartWindow                                                                        time.Duration
	P99Window, BaselineTicks, AuditRing, TimelineRing, SnapshotEvents, AggregateMin, ViewerQueue int
	DrainSlack                                                                                   int // affected backlog at or under this counts as drained (Workers × ClaimBatch: rows in flight)
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

// Registry holds every aggregate, the audit rings, the histograms, the timeline and the SSE hub.
type Registry struct {
	cfg   Config
	hub   *hub
	ticks atomic.Int64
	ln15  float64

	ingest     [reasonCount]atomic.Int64 // per tick
	delivered  atomic.Int64              // per tick
	kms        [3]atomic.Int64           // per tick: ok, transient, deny
	kmsProv    []atomic.Int64            // per tick, per provider: every unwrap/generate (the no-cache summary lines)
	rejWithin  atomic.Int64              // per tick: admission rejections of within-share tenants (the L4 split)
	rejOver    atomic.Int64              // per tick: admission rejections of over-share tenants
	overWithin atomic.Int64              // cumulative: overloaded rejections of within-share tenants (L4 judges it)

	affected []atomic.Bool // the scenario's declared set, read lock-free at record time (Q8)
	hists    [2][]hist     // per class (0 healthy, 1 affected): P99Window tick slots
	slot     atomic.Int32  // the slot Delivered writes

	mu        sync.Mutex
	audits    [][]Audit // per tenant ring
	auditHead []int
	auditN    []int
	states    []uint8
	grid      []byte
	timeline  []event // ring of TimelineRing entries
	tlHead    int
	tlN       int
	tlSeq     int64
	scen      scenarioState   // the running scenario's card state
	targets   []int           // replaced whole by SetTargets, never mutated: safe to read after a copy under mu
	p99Ring   []time.Duration // len BaselineTicks: the windowed healthy p99 as the snapshot reports it each tick
	p99Head   int
	p99N      int
	baseline  time.Duration // frozen by SetTargets: mean of p99Ring
	pendTrans []Audit       // state transitions other than ACTIVE↔RIDING_THROUGH and REVOKED, flushed per tick
	rt        []rtCount     // per provider ACTIVE↔RIDING_THROUGH counts, flushed once per second
	rtFlushed time.Time
	last      Reading // the last tick's tiles, for scenario summary lines
	peak      Reading // per-field maxima since ResetPeaks, for the same lines

	verdicts    map[string]verdict // the checker's lights (M5), the snapshot's "invariants" key
	detected    []detection        // per tenant: the first REVOKED audit of the current episode (S3's detection line)
	lastHealthy time.Duration      // the last tick's windowed healthy p99 (L1's current value)
	delRing     []int64            // the last P99Window ticks' delivered counts (L4's DeliveredPS)
	delHead     int
	healthyRej  atomic.Int64 // rejections dealt to unaffected tenants since SetTargets (L1)
}

// verdict is one invariant light as the snapshot carries it (the page reads ok, n, at, detail).
type verdict struct {
	OK     bool   `json:"ok"`
	N      int64  `json:"n"`
	At     int64  `json:"at"`
	Detail string `json:"detail"`
}

// detection is the service's first REVOKED transition of one revocation episode; the episode ends on the
// transition out of REVOKED, and the next REVOKED starts a new one.
type detection struct {
	at     time.Time
	purged int
	open   bool
	ok     bool
}

// Report stores a verdict (check.Reporter); the next snapshot carries it under "invariants".
func (r *Registry) Report(id string, ok bool, count int64, at time.Time, detail string) {
	r.mu.Lock()
	if r.verdicts == nil {
		r.verdicts = make(map[string]verdict)
	}
	r.verdicts[id] = verdict{OK: ok, N: count, At: at.UnixMilli(), Detail: detail}
	r.mu.Unlock()
}

// DetectedRevokedAt is the service's first REVOKED audit of the tenant's current or last revocation episode (the
// checker pairs it with ground truth for the "detected in x s" line).
func (r *Registry) DetectedRevokedAt(idx int) (at time.Time, purged int, ok bool) {
	if idx < 0 || idx >= len(r.detected) {
		return time.Time{}, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.detected[idx]
	return d.at, d.purged, d.ok
}

// HealthyP99 is L1's pair: the last tick's windowed healthy p99 (M4's interpolated read) and the baseline frozen at
// SetTargets; ok while a scenario runs with a frozen baseline.
func (r *Registry) HealthyP99() (current, baseline time.Duration, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastHealthy, r.baseline, r.scen.name != "" && r.baseline > 0
}

// HealthyRejections counts every non-accepted ingest outcome dealt to a tenant outside the affected set since the
// last SetTargets (L1: unaffected tenants see no rejections).
func (r *Registry) HealthyRejections() int64 { return r.healthyRej.Load() }

// DeliveredPS is the delivery rate over the last P99Window ticks (L4 reads it instead of one tick's value).
func (r *Registry) DeliveredPS() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sum int64
	for _, v := range r.delRing {
		sum += v
	}
	return float64(sum) / (float64(len(r.delRing)) * r.cfg.Interval.Seconds())
}

// Reading is the last tick's headline numbers as a scenario's summary line reads them.
type Reading struct {
	DeliveredPS, KMSCallsPS, AcceptedPS float64
	HealthyP99Ms, AffectedP99Ms         float64
	Inflight                            map[string]int
	ByState                             [4]int
	Backlog                             int
	ProviderCallsPS                     map[string]float64
}

// ResetPeaks starts a new peak window (a scenario phase); Peaks returns the per-field maxima since (Inflight and
// ProviderCallsPS hold each key's maximum, not one tick's).
func (r *Registry) ResetPeaks() {
	r.mu.Lock()
	r.peak = Reading{Inflight: map[string]int{}, ProviderCallsPS: map[string]float64{}}
	r.mu.Unlock()
}

// Peaks returns the maxima since ResetPeaks.
func (r *Registry) Peaks() Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.peak
	out.Inflight = maps.Clone(r.peak.Inflight)
	out.ProviderCallsPS = maps.Clone(r.peak.ProviderCallsPS)
	return out
}

// Last returns the last tick's Reading (zero before the first tick).
func (r *Registry) Last() Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.last
	out.Inflight = maps.Clone(r.last.Inflight)
	out.ProviderCallsPS = maps.Clone(r.last.ProviderCallsPS)
	return out
}

// ProviderCallsPS is the last tick's KMS calls per second made for one provider's tenants (all classes).
func (r *Registry) ProviderCallsPS(provider string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last.ProviderCallsPS[provider]
}

// rtCount buffers the ride-through churn of one provider.
type rtCount struct{ up, down int }

// scenarioState is what the card reads; name "" means idle. clearedAt is the fault-cleared instant from which
// recovery (every target ACTIVE) and drain (affected backlog ≤ DrainSlack) are measured.
type scenarioState struct {
	name, phase                       string
	endsAt, since                     time.Time
	clearedAt, recoveredAt, drainedAt time.Time
}

// event is one timeline line as the snapshot carries it.
type event struct {
	Seq  int64  `json:"seq"`
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// New builds the registry.
func New(cfg Config) *Registry {
	if cfg.AuditRing <= 0 {
		cfg.AuditRing = 16
	}
	if cfg.TimelineRing <= 0 {
		cfg.TimelineRing = 200
	}
	if cfg.SnapshotEvents <= 0 {
		cfg.SnapshotEvents = 20
	}
	if cfg.P99Window <= 0 {
		cfg.P99Window = 10
	}
	if cfg.BaselineTicks <= 0 {
		cfg.BaselineTicks = 20
	}
	if cfg.AggregateMin <= 0 {
		cfg.AggregateMin = 3
	}
	n := len(cfg.Tenants)
	r := &Registry{cfg: cfg, hub: newHub(cfg.ViewerQueue, cfg.Watcher), ln15: 0.4054651081081644, audits: make([][]Audit, n), auditHead: make([]int, n), auditN: make([]int, n), states: make([]uint8, n), grid: make([]byte, n), timeline: make([]event, cfg.TimelineRing), affected: make([]atomic.Bool, n), p99Ring: make([]time.Duration, cfg.BaselineTicks), rt: make([]rtCount, len(cfg.Providers)), kmsProv: make([]atomic.Int64, len(cfg.Providers)), detected: make([]detection, n), delRing: make([]int64, cfg.P99Window)}
	for c := range r.hists {
		r.hists[c] = make([]hist, cfg.P99Window)
	}
	return r
}

// provider is the provider index of a grid index (contiguous bands).
func (r *Registry) provider(idx int) int {
	if len(r.cfg.Providers) == 0 || len(r.cfg.Tenants) == 0 {
		return 0
	}
	return idx * len(r.cfg.Providers) / len(r.cfg.Tenants)
}

// Timeline appends a free-text line (scenario markers, the restored line, the checker in M5).
func (r *Registry) Timeline(text string) {
	r.mu.Lock()
	r.post(r.cfg.Clock.Now(), text)
	r.mu.Unlock()
}

// post appends one timeline line; caller holds r.mu.
func (r *Registry) post(at time.Time, text string) {
	r.tlSeq++
	r.timeline[r.tlHead] = event{Seq: r.tlSeq, At: at.UnixMilli(), Text: text}
	r.tlHead = (r.tlHead + 1) % len(r.timeline)
	if r.tlN < len(r.timeline) {
		r.tlN++
	}
}

// events returns the newest n timeline lines, newest first; caller holds r.mu.
func (r *Registry) events(n int) []event {
	if n > r.tlN {
		n = r.tlN
	}
	out := make([]event, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.timeline[(r.tlHead-i+len(r.timeline))%len(r.timeline)])
	}
	return out
}

// stateNames mirrors cmek.State without importing it (metrics never imports the core).
func stateName(s uint8) string {
	switch s {
	case 0:
		return "ACTIVE"
	case 1:
		return "RIDING_THROUGH"
	case 2:
		return "KEY_UNAVAILABLE"
	case 3:
		return "REVOKED"
	}
	return "?"
}

// Ingest counts one outcome by reason and, for the three admission reasons only, by share class: the L4 split.
// KMS-fault reasons are not fair-share shedding and stay out of the split.
func (r *Registry) Ingest(idx int, reason string, withinShare bool) {
	i := reasonIndex(reason)
	r.ingest[i].Add(1)
	if i != reasonAccepted && idx >= 0 && idx < len(r.affected) && !r.affected[idx].Load() {
		r.healthyRej.Add(1)
	}
	switch i {
	case reasonRateLimited, reasonBacklogFull, reasonOverloaded:
		if withinShare {
			r.rejWithin.Add(1)
			if i == reasonOverloaded {
				r.overWithin.Add(1)
			}
		} else {
			r.rejOver.Add(1)
		}
	}
}

// OverloadedWithinShare is the cumulative count of overloaded rejections dealt to within-share tenants (L4: must stay 0).
func (r *Registry) OverloadedWithinShare() int64 { return r.overWithin.Load() }

// SetScenario publishes the card countdown; an empty name clears it (and the recovery marks); a zero endsAt means
// an open-ended phase (ends_at null).
func (r *Registry) SetScenario(name, phase string, endsAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		r.scen = scenarioState{}
		return
	}
	if r.scen.name != name {
		r.scen.since = r.cfg.Clock.Now()
	}
	r.scen.name, r.scen.phase, r.scen.endsAt = name, phase, endsAt
}

// Scenario reports the running scenario, if any, and when it started.
func (r *Registry) Scenario() (name string, running bool, since time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scen.name, r.scen.name != "", r.scen.since
}

// SetTargets replaces the affected set and freezes the baseline (mean of the last BaselineTicks windowed healthy
// p99s, whatever the ring holds so far); only scenarios call it.
func (r *Registry) SetTargets(idx []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, i := range r.targets {
		r.affected[i].Store(false)
	}
	r.targets = append([]int(nil), idx...)
	for _, i := range r.targets {
		if i >= 0 && i < len(r.affected) {
			r.affected[i].Store(true)
		}
	}
	var sum time.Duration
	for i := 0; i < r.p99N; i++ {
		sum += r.p99Ring[i]
	}
	r.baseline = 0
	if r.p99N > 0 {
		r.baseline = sum / time.Duration(r.p99N)
	}
	r.healthyRej.Store(0)
	r.scen.clearedAt, r.scen.recoveredAt, r.scen.drainedAt = time.Time{}, time.Time{}, time.Time{}
}

// ClearTargets ends the affected set (the baseline stays until the next SetTargets).
func (r *Registry) ClearTargets() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, i := range r.targets {
		r.affected[i].Store(false)
	}
	r.targets = nil
}

// Affected reports the tenant's affected bit (lock-free atomic load); world.Tenant reads it.
func (r *Registry) Affected(idx int) bool {
	if idx < 0 || idx >= len(r.affected) {
		return false
	}
	return r.affected[idx].Load()
}

// SetCleared marks the fault-cleared instant from which recovery_s and drain_s are measured.
func (r *Registry) SetCleared(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scen.clearedAt, r.scen.recoveredAt, r.scen.drainedAt = at, time.Time{}, time.Time{}
}

// Recovered reports whether, since SetCleared, every target has been seen ACTIVE and the affected backlog at or
// under DrainSlack (tick step 5 stamps both), and the two durations (zero while unstamped).
func (r *Registry) Recovered() (recovered, drained time.Duration, done bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.scen.recoveredAt.IsZero() {
		recovered = r.scen.recoveredAt.Sub(r.scen.clearedAt)
	}
	if !r.scen.drainedAt.IsZero() {
		drained = r.scen.drainedAt.Sub(r.scen.clearedAt)
	}
	return recovered, drained, !r.scen.recoveredAt.IsZero() && !r.scen.drainedAt.IsZero()
}

// Delivered records one end-to-end latency in the tick histogram of the tenant's class (queue.Recorder).
func (r *Registry) Delivered(idx int, latency time.Duration) {
	r.delivered.Add(1)
	class := 0
	if r.Affected(idx) {
		class = 1
	}
	r.record(class, latency)
}

// Audit stores the entry in the tenant ring, counts KMS calls by class, and turns a state change into a timeline
// line: a REVOKED transition posts at once with its purge count (never aggregated); ACTIVE↔RIDING_THROUGH churn is
// counted per provider and flushed once per second as one line; every other transition waits for the next
// per-second flush, where AggregateMin or more of the same (provider, from, to) collapse to one line and the rest
// keep the M2 form (a single waits ≤ 1 s).
func (r *Registry) Audit(e Audit) {
	if e.Idx < 0 || e.Idx >= len(r.audits) {
		return
	}
	if e.Op == "unwrap" || e.Op == "generate" {
		if e.Class < 3 {
			r.kms[e.Class].Add(1)
		}
		r.kmsProv[r.provider(e.Idx)].Add(1)
	}
	r.mu.Lock()
	if e.Op == "state" {
		d := &r.detected[e.Idx]
		if e.To == 3 && !d.open {
			*d = detection{at: e.At, purged: e.Purged, open: true, ok: true}
		} else if e.From == 3 && e.To != 3 {
			d.open = false
		}
		switch {
		case e.To == 3:
			r.post(e.At, fmt.Sprintf("%s REVOKED, %d DEKs purged", r.cfg.Tenants[e.Idx], e.Purged))
		case e.From == 0 && e.To == 1:
			r.rt[r.provider(e.Idx)].up++
		case e.From == 1 && e.To == 0:
			r.rt[r.provider(e.Idx)].down++
		default:
			r.pendTrans = append(r.pendTrans, e)
		}
	}
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

// TenantAudit returns the last n ring entries for one tenant, newest first.
func (r *Registry) TenantAudit(idx, n int) []Audit {
	out := []Audit{}
	if idx < 0 || idx >= len(r.audits) {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.auditN[idx] {
		n = r.auditN[idx]
	}
	ring := r.audits[idx]
	for i := 1; i <= n; i++ {
		out = append(out, ring[(r.auditHead[idx]-i+r.cfg.AuditRing)%r.cfg.AuditRing])
	}
	return out
}

// TenantCallsPerMin counts unwrap/generate entries newer than 60 s in the tenant's ring (a lower bound when the
// ring covers less than 60 s).
func (r *Registry) TenantCallsPerMin(idx int) float64 {
	if idx < 0 || idx >= len(r.audits) {
		return 0
	}
	cut := r.cfg.Clock.Now().Add(-time.Minute)
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for i := 0; i < r.auditN[idx]; i++ {
		e := r.audits[idx][i]
		if (e.Op == "unwrap" || e.Op == "generate") && e.At.After(cut) {
			n++
		}
	}
	return float64(n)
}

// flushTransitions turns the buffered transitions into timeline lines (step 6) once per second, so an outage
// posts at most one line per (provider, from, to) per second plus its singles; caller holds r.mu. Every line a
// flush posts carries the flush instant (a buffered single is published up to 1 s after its transition), so the
// timeline's timestamps never run backwards within a flush.
func (r *Registry) flushTransitions(now time.Time) {
	if now.Sub(r.rtFlushed) < time.Second {
		return
	}
	r.rtFlushed = now
	if len(r.pendTrans) > 0 {
		type key struct{ prov, from, to uint8 }
		counts := map[key]int{}
		for _, e := range r.pendTrans {
			counts[key{uint8(r.provider(e.Idx)), e.From, e.To}]++
		}
		posted := map[key]bool{}
		for _, e := range r.pendTrans {
			k := key{uint8(r.provider(e.Idx)), e.From, e.To}
			switch {
			case counts[k] >= r.cfg.AggregateMin && !posted[k]:
				posted[k] = true
				r.post(now, fmt.Sprintf("%d %s tenants %s → %s", counts[k], r.cfg.Providers[k.prov], stateName(e.From), stateName(e.To)))
			case counts[k] < r.cfg.AggregateMin:
				r.post(now, fmt.Sprintf("%s %s → %s (%s)", r.cfg.Tenants[e.Idx], stateName(e.From), stateName(e.To), e.Detail))
			}
		}
		r.pendTrans = r.pendTrans[:0]
	}
	for p := range r.rt {
		c := r.rt[p]
		if c.up == 0 && c.down == 0 {
			continue
		}
		r.rt[p] = rtCount{}
		r.post(now, fmt.Sprintf("%s: %d ACTIVE → RIDING_THROUGH, %d back", r.cfg.Providers[p], c.up, c.down))
	}
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
		DeliveredPS  float64 `json:"delivered_ps"`
		CapacityPS   float64 `json:"capacity_ps"`
		OfferedPS    float64 `json:"offered_ps"`
		HealthyP99Ms float64 `json:"healthy_p99_ms"`
		KMSCallsPS   float64 `json:"kms_calls_ps"`
		ByState      [4]int  `json:"by_state"`
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
	L4 struct {
		RejectedWithinSharePS float64 `json:"rejected_within_share_ps"`
		RejectedOverSharePS   float64 `json:"rejected_over_share_ps"`
	} `json:"l4"`
	P99Ms struct {
		Healthy  float64 `json:"healthy"`
		Affected float64 `json:"affected"`
		Baseline float64 `json:"baseline"`
	} `json:"p99_ms"`
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
	Inflight map[string]int `json:"inflight"`
	Scenario struct {
		Name      string   `json:"name"`
		Phase     string   `json:"phase"`
		EndsAt    *int64   `json:"ends_at"`
		RecoveryS *float64 `json:"recovery_s"`
		DrainS    *float64 `json:"drain_s"`
	} `json:"scenario"`
	Grid       string             `json:"grid"`
	Events     []event            `json:"events"`
	Invariants map[string]verdict `json:"invariants,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// tick builds one snapshot (ARCH › (f)): every source is read before reg.mu (states, backlog totals, the affected
// backlog sum over the targets, in-flight per provider), per-tick atomics are swapped, the p99 window rolls,
// recovery is stamped from the locals, buffered transitions are flushed, and the encoded bytes go to the hub.
func (r *Registry) tick() {
	tick := r.ticks.Add(1)
	now := r.cfg.Clock.Now()
	perSec := float64(time.Second) / float64(r.cfg.Interval)
	// step 2: sources, with no metrics lock held
	r.mu.Lock()
	targets := r.targets
	r.mu.Unlock()
	states := r.states // written only by this goroutine
	r.cfg.Grid.States(states)
	total := r.cfg.Backlog.Total()
	affectedSum, allActive := 0, true
	for _, i := range targets {
		affectedSum += r.cfg.Backlog.Backlog(i)
		allActive = allActive && states[i] == 0
	}
	inflight := make(map[string]int, len(r.cfg.Providers))
	for _, p := range r.cfg.Providers {
		if r.cfg.Inflight != nil {
			inflight[p] = r.cfg.Inflight.Inflight(p)
		} else {
			inflight[p] = 0
		}
	}
	offered := 0.0
	if r.cfg.Offered != nil {
		offered = r.cfg.Offered.OfferedPS()
	}
	healthy, affected := r.roll()
	r.mu.Lock()
	defer r.mu.Unlock()
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
	s.Tiles.OfferedPS = offered
	s.Tiles.KMSCallsPS = float64(k[0]+k[1]+k[2]) * perSec
	s.Tiles.HealthyP99Ms = ms(healthy)
	for i, st := range states {
		if st < 4 {
			s.Tiles.ByState[st]++
		}
		c := '0' + st
		if r.affected[i].Load() {
			c += 4
		}
		r.grid[i] = c
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
	s.L4.RejectedWithinSharePS = float64(r.rejWithin.Swap(0)) * perSec
	s.L4.RejectedOverSharePS = float64(r.rejOver.Swap(0)) * perSec
	// the healthy ring feeds the next baseline
	r.p99Ring[r.p99Head] = healthy
	r.p99Head = (r.p99Head + 1) % len(r.p99Ring)
	if r.p99N < len(r.p99Ring) {
		r.p99N++
	}
	s.P99Ms.Healthy, s.P99Ms.Affected, s.P99Ms.Baseline = ms(healthy), ms(affected), ms(r.baseline)
	r.lastHealthy = healthy
	r.delRing[r.delHead] = del
	r.delHead = (r.delHead + 1) % len(r.delRing)
	// step 5: recovery bookkeeping from the locals
	if !r.scen.clearedAt.IsZero() {
		if r.scen.recoveredAt.IsZero() && allActive {
			r.scen.recoveredAt = now
		}
		if r.scen.drainedAt.IsZero() && affectedSum <= r.cfg.DrainSlack {
			r.scen.drainedAt = now
		}
	}
	s.Scenario.Name, s.Scenario.Phase = r.scen.name, r.scen.phase
	if r.scen.name != "" {
		if !r.scen.endsAt.IsZero() {
			ends := r.scen.endsAt.UnixMilli()
			s.Scenario.EndsAt = &ends
		}
		if !r.scen.recoveredAt.IsZero() {
			v := r.scen.recoveredAt.Sub(r.scen.clearedAt).Seconds()
			s.Scenario.RecoveryS = &v
		}
		if !r.scen.drainedAt.IsZero() {
			v := r.scen.drainedAt.Sub(r.scen.clearedAt).Seconds()
			s.Scenario.DrainS = &v
		}
	}
	s.Backlog.Total, s.Backlog.Affected = total, affectedSum
	s.Inflight = inflight
	r.last = Reading{DeliveredPS: s.Tiles.DeliveredPS, KMSCallsPS: s.Tiles.KMSCallsPS, AcceptedPS: s.IngestPS.Accepted, HealthyP99Ms: s.P99Ms.Healthy, AffectedP99Ms: s.P99Ms.Affected,
		Inflight: maps.Clone(inflight), ByState: s.Tiles.ByState, Backlog: total, ProviderCallsPS: make(map[string]float64, len(r.cfg.Providers))}
	for i, p := range r.cfg.Providers {
		r.last.ProviderCallsPS[p] = float64(r.kmsProv[i].Swap(0)) * perSec
	}
	if r.peak.Inflight != nil { // a peak window is open
		pk := &r.peak
		pk.DeliveredPS, pk.KMSCallsPS, pk.AcceptedPS = max(pk.DeliveredPS, r.last.DeliveredPS), max(pk.KMSCallsPS, r.last.KMSCallsPS), max(pk.AcceptedPS, r.last.AcceptedPS)
		pk.HealthyP99Ms, pk.AffectedP99Ms, pk.Backlog = max(pk.HealthyP99Ms, r.last.HealthyP99Ms), max(pk.AffectedP99Ms, r.last.AffectedP99Ms), max(pk.Backlog, total)
		for i := range pk.ByState {
			pk.ByState[i] = max(pk.ByState[i], s.Tiles.ByState[i])
		}
		for p, v := range inflight {
			pk.Inflight[p] = max(pk.Inflight[p], v)
		}
		for p, v := range r.last.ProviderCallsPS {
			pk.ProviderCallsPS[p] = max(pk.ProviderCallsPS[p], v)
		}
	}
	// step 6: transitions
	r.flushTransitions(now)
	s.Grid = string(r.grid)
	s.Events = r.events(r.cfg.SnapshotEvents)
	if len(r.verdicts) > 0 {
		s.Invariants = maps.Clone(r.verdicts)
	}
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
