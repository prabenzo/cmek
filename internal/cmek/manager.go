// Owner: Claude (reviewed by Ben)
package cmek

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/prabenzo/cmek/internal/logx"
)

// Manager is the CMEK core: one lease, DEK cache and key state machine per tenant, each behind its own mutex;
// one fetcher (provider bulkheads, singleflight) shared by all. It never stores plaintext anywhere but memory,
// never runs SQL, and never calls a KMS from the delivery path.
type Manager struct {
	tenants  map[string]*tenant
	order    []*tenant                // grid order; fixed at New
	states   []atomic.Uint32          // the grid array, written under the owning t.mu, read lock-free by States
	provSem  map[string]chan struct{} // per-provider bulkhead, cap ProviderInflight
	inflight map[string]*atomic.Int64 // calls inside each provider (the slow-KMS tile)
	sf       singleflight.Group
	ctx      context.Context
	cfg      Config
	log      logx.Throttle
}

// New builds a Manager; ctx bounds every fetch. Zero durations take the spec's demo values.
func New(ctx context.Context, cfg Config) *Manager {
	if cfg.Spawn == nil {
		cfg.Spawn = func(f func()) { go f() }
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Lease, 30*time.Second)
	def(&cfg.SoftTTL, 15*time.Second)
	def(&cfg.EarlyExpiry, time.Second)
	def(&cfg.KMSTimeout, 500*time.Millisecond)
	def(&cfg.BackoffMin, 500*time.Millisecond)
	def(&cfg.BackoffMax, 8*time.Second)
	def(&cfg.RevokedReprobe, 5*time.Second)
	def(&cfg.DEKMaxAge, 10*time.Minute)
	def(&cfg.SweepInterval, 250*time.Millisecond)
	if cfg.DEKMaxMessages <= 0 {
		cfg.DEKMaxMessages = 10000
	}
	if cfg.ProviderInflight <= 0 {
		cfg.ProviderInflight = 32
	}
	if cfg.IngestWaiters <= 0 {
		cfg.IngestWaiters = 4
	}
	m := &Manager{tenants: make(map[string]*tenant, len(cfg.Tenants)), states: make([]atomic.Uint32, len(cfg.Tenants)), provSem: make(map[string]chan struct{}), inflight: make(map[string]*atomic.Int64), ctx: ctx, cfg: cfg, log: logx.Throttle{Log: cfg.Logger}}
	for _, p := range cfg.Providers {
		m.provider(p)
	}
	for i, spec := range cfg.Tenants {
		m.provider(spec.Provider)
		t := &tenant{idx: i, spec: spec, deks: make(map[string]*dek), pending: make(map[string]time.Time), lease: Lease{TTL: cfg.Lease, SoftTTL: cfg.SoftTTL, Early: cfg.EarlyExpiry}}
		m.tenants[spec.ID] = t
		m.order = append(m.order, t)
	}
	return m
}

func (m *Manager) provider(p string) {
	if _, ok := m.provSem[p]; !ok {
		m.provSem[p] = make(chan struct{}, m.cfg.ProviderInflight)
		m.inflight[p] = new(atomic.Int64)
	}
}

// exhausted reports whether the active DEK has reached its message or age bound (rotation while ACTIVE).
func (m *Manager) exhausted(d *dek, now time.Time) bool {
	return d.msgs >= m.cfg.DEKMaxMessages || now.Sub(d.createdAt) >= m.cfg.DEKMaxAge
}

// SetPassThrough switches one tenant between the cached design (off) and per-request KMS calls (on): the no-cache
// demo. Entering pass-through drops every cached primitive (audit purge "cache off"); leaving it lets the next
// probe or renewal fill the cache again. Everything else (state machine, backoff, bulkheads, the Deny row and the
// stale-OK guard) is unchanged, so the runs isolate the cache as the one variable.
func (m *Manager) SetPassThrough(id string, on bool) {
	t := m.tenants[id]
	if t == nil {
		return
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.passThrough == on {
		return
	}
	t.passThrough = on
	if on {
		if purged := t.purge(); purged > 0 {
			m.audit(Audit{At: now, Tenant: t.spec.ID, Op: "purge", Outcome: "cache off", Detail: "per-request KMS calls", Purged: purged})
		}
	}
}

// passThroughEncrypt is EncryptKey without the cache: one KMS call (a generate for a tenant with no DEK or an
// exhausted one, else an unwrap of the active DEK) made directly, outside singleflight, and its primitive handed
// to the caller once. Called with t.mu released, after the parked-state returns.
func (m *Manager) passThroughEncrypt(t *tenant) (Handle, error) {
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	d := t.active
	gen := d == nil || (t.state == Active && m.exhausted(d, now))
	t.mu.Unlock()
	var r result
	if gen {
		d, r = m.generate(t)
	} else {
		r = m.call(t, "unwrap", d)
		m.apply(t, "unwrap", d, r)
	}
	return m.passThroughHandle(t, d, r, true)
}

// passThroughHandle maps one direct call's result to a Handle or the caller's sentinel; count is true for a seal.
func (m *Manager) passThroughHandle(t *tenant, d *dek, r result, count bool) (Handle, error) {
	switch {
	case r.err == nil:
		now := m.cfg.Clock.Now()
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.state == Revoked {
			return Handle{}, ErrKeyRevoked // a stale OK: the deny won
		}
		if !t.lease.Usable(now) {
			return Handle{}, ErrKeyUnavailable
		}
		if count {
			d.msgs++
		}
		return Handle{DEKID: d.id, ValidUntil: t.lease.Until(), prim: r.prim}, nil
	case r.class == Deny:
		return Handle{}, ErrKeyRevoked
	case errors.Is(r.err, errStore):
		return Handle{}, r.err
	default:
		return Handle{}, ErrKeyUnavailable
	}
}

// handle wraps the active primitive in a Handle when the tenant may seal now; caller holds t.mu.
func (m *Manager) handle(t *tenant, now time.Time) (Handle, bool) {
	d := t.active
	if d == nil || d.prim == nil || t.passThrough || !t.lease.Usable(now) || (t.state == Active && m.exhausted(d, now)) {
		return Handle{}, false
	}
	d.msgs++
	return Handle{DEKID: d.id, ValidUntil: t.lease.Until(), prim: d.prim}, true
}

// EncryptKey returns a handle on the tenant's active DEK, fetching synchronously on the cold path; parked tenants
// get their sentinel without a KMS call. A hot ACTIVE tenant whose lease is soft-due kicks one lazy renewal.
func (m *Manager) EncryptKey(ctx context.Context, id string) (Handle, error) {
	t := m.tenants[id]
	if t == nil {
		return Handle{}, ErrKeyUnavailable
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	switch t.state {
	case Revoked:
		t.mu.Unlock()
		return Handle{}, ErrKeyRevoked
	case KeyUnavailable:
		t.mu.Unlock()
		return Handle{}, ErrKeyUnavailable
	}
	if t.passThrough {
		t.mu.Unlock()
		return m.passThroughEncrypt(t)
	}
	if h, ok := m.handle(t, now); ok { // hot path
		kick := t.state == Active && t.lease.SoftDue(now) && !t.probing
		if kick {
			t.probing = true
		}
		t.mu.Unlock()
		if kick {
			m.cfg.Spawn(func() { m.probe(t) })
		}
		return h, nil
	}
	// cold path: no usable lease, no DEK, or an exhausted active DEK while ACTIVE
	if t.waiters >= m.cfg.IngestWaiters {
		t.mu.Unlock()
		return Handle{}, ErrKeyUnavailable
	}
	t.waiters++
	t.mu.Unlock()
	err := m.renew(t, true)
	now = m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.waiters--
	if t.state == Revoked {
		return Handle{}, ErrKeyRevoked
	}
	if h, ok := m.handle(t, now); ok {
		return h, nil
	}
	if errors.Is(err, errStore) {
		return Handle{}, err // a store failure is not a key-state answer: 500 internal, not 503
	}
	return Handle{}, ErrKeyUnavailable
}

// DecryptKey checks the lease now and returns a handle on the named DEK; ErrPoison for a dek_id the tenant does not
// own [SC-F8]; it never blocks and never calls a KMS, except for a tenant in pass-through (the no-cache demo), where
// it makes one unwrap call per message and a failed call parks the tenant and answers ErrKeyUnavailable.
func (m *Manager) DecryptKey(id, dekID string) (Handle, error) {
	t := m.tenants[id]
	if t == nil {
		return Handle{}, ErrPoison
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	d := t.deks[dekID]
	if d == nil {
		t.mu.Unlock()
		return Handle{}, ErrPoison
	}
	switch t.state {
	case Revoked:
		t.mu.Unlock()
		return Handle{}, ErrKeyRevoked
	case KeyUnavailable:
		t.mu.Unlock()
		return Handle{}, ErrLeaseExpired
	}
	if t.passThrough {
		t.mu.Unlock()
		r := m.call(t, "unwrap", d)
		m.apply(t, "unwrap", d, r)
		return m.passThroughHandle(t, d, r, false)
	}
	defer t.mu.Unlock()
	if !t.lease.Usable(now) {
		return Handle{}, ErrLeaseExpired
	}
	if d.prim == nil {
		return Handle{}, ErrDEKCold
	}
	return Handle{DEKID: dekID, ValidUntil: t.lease.Until(), prim: d.prim}, nil
}

// Hot reports whether the scheduler may dispatch the tenant now (ACTIVE or RIDING_THROUGH, usable lease, no pending
// unwrap); an ACTIVE tenant with backlog whose lease is soft-due or lapsed gets one lazy renewal (the L3 kick).
func (m *Manager) Hot(id string) bool {
	t := m.tenants[id]
	if t == nil {
		return false
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	if t.passThrough { // no lease to check and nothing to kick: the worker's DecryptKey is the call
		hot := t.state == Active
		t.mu.Unlock()
		return hot
	}
	usable := t.lease.Usable(now)
	hot := (t.state == Active || t.state == RidingThrough) && usable && len(t.pending) == 0
	kick := t.state == Active && !t.probing && (!usable || t.lease.SoftDue(now))
	if kick {
		t.probing = true
	}
	t.mu.Unlock()
	if kick {
		m.cfg.Spawn(func() { m.probe(t) })
	}
	return hot
}

// Warm records that a worker needs a purged DEK and, when no probe or warm is running, starts one unwrap; retries
// follow the backoff schedule from Tick. Hot stays false for the tenant until the unwrap succeeds or is denied [SC-F5].
func (m *Manager) Warm(id, dekID string) {
	t := m.tenants[id]
	if t == nil {
		return
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	d := t.deks[dekID]
	if d == nil || t.passThrough { // pass-through: nothing is warmed, every DecryptKey calls
		t.mu.Unlock()
		return
	}
	if _, ok := t.pending[dekID]; ok {
		t.mu.Unlock()
		return
	}
	t.pending[dekID] = now
	kick := !t.probing && (t.state == Active || t.state == RidingThrough) && t.lease.Usable(now)
	if kick {
		t.probing = true
	}
	t.mu.Unlock()
	if kick {
		m.cfg.Spawn(func() { m.warm(t, d) })
	}
}

// Tick runs lease expiry, purges, DEK ageing, due probes and due warm retries for every tenant; the World sweep
// calls it every SweepInterval. It holds one t.mu at a time and spawns that tenant's due work after its unlock.
func (m *Manager) Tick(now time.Time) {
	for _, t := range m.order {
		var due func()
		t.mu.Lock()
		// 1. lease lapse: drop every plaintext; RIDING_THROUGH fails closed, ACTIVE stays ACTIVE (idle lapse)
		if !t.lease.SentAt.IsZero() && !t.lease.Usable(now) && t.hot() > 0 {
			purged := t.purge()
			if t.state == RidingThrough {
				m.setState(t, KeyUnavailable, now, fmtPurged("lease expired", purged), purged)
			} else {
				m.audit(Audit{At: now, Tenant: t.spec.ID, Op: "purge", Outcome: "idle", Detail: "lease lapsed with no traffic", Purged: purged})
			}
		}
		// 2. DEK ageing applies only to non-active DEKs [SC-F3]
		for _, d := range t.deks {
			if d != t.active && d.prim != nil && now.Sub(d.hotSince) >= m.cfg.DEKMaxAge+m.cfg.Lease {
				d.prim = nil
				m.audit(Audit{At: now, Tenant: t.spec.ID, Op: "purge", Outcome: "aged", Detail: d.id, Purged: 1})
			}
		}
		// 3. due probe (never for an ACTIVE tenant: Q4) or due warm
		switch {
		case (t.state == RidingThrough || t.state == KeyUnavailable || t.state == Revoked) && !t.probing && !now.Before(t.nextProbeAt):
			t.probing = true
			due = func() { m.probe(t) }
		case (t.state == Active || t.state == RidingThrough) && t.lease.Usable(now) && !t.probing:
			for id, at := range t.pending {
				if d := t.deks[id]; d != nil && !now.Before(at) {
					t.probing = true
					due = func() { m.warm(t, d) }
					break
				}
			}
		}
		t.mu.Unlock()
		if due != nil {
			m.cfg.Spawn(due)
		}
	}
}

func fmtPurged(why string, n int) string { return why + ", " + strconv.Itoa(n) + " DEKs purged" }

// Info returns the tenant read model.
func (m *Manager) Info(id string) TenantInfo {
	t := m.tenants[id]
	if t == nil {
		return TenantInfo{}
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	info := TenantInfo{State: t.state, DEKs: len(t.deks), HotDEKs: t.hot(), Pending: len(t.pending), Attempt: t.attempt, Waiters: t.waiters, Probing: t.probing}
	if !t.lease.SentAt.IsZero() {
		info.LeaseAge = now.Sub(t.lease.SentAt)
		if rem := t.lease.Remaining(now); rem > 0 {
			info.LeaseRemaining = rem
		}
	}
	if t.state != Active && t.nextProbeAt.After(now) {
		info.NextProbeIn = t.nextProbeAt.Sub(now)
	}
	if t.active != nil {
		info.ActiveDEK = t.active.id
	}
	return info
}

// States copies each tenant's State into dst in grid order without taking any tenant lock.
func (m *Manager) States(dst []State) {
	for i := range dst {
		if i < len(m.states) {
			dst[i] = State(m.states[i].Load())
		}
	}
}

// Inflight returns the KMS calls currently inside a provider (for the slow-KMS tile).
func (m *Manager) Inflight(provider string) int {
	if c := m.inflight[provider]; c != nil {
		return int(c.Load())
	}
	return 0
}
