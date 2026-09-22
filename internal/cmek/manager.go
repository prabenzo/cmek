// Owner: Claude (reviewed by Ben)
package cmek

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// throttle logs one line per second per message (the core has no logx dependency by the import rule).
type throttle struct {
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *throttle) Error(msg string, attrs ...any) {
	if t.log == nil {
		return
	}
	t.mu.Lock()
	now := time.Now()
	ok := now.Sub(t.last[msg]) >= time.Second
	if ok {
		t.last[msg] = now
	}
	t.mu.Unlock()
	if ok {
		t.log.Error(msg, attrs...)
	}
}

// errStore marks a DEKStore failure: not a KMS outcome, so no audit line, no state change, and EncryptKey returns it
// unclassified (world.Outcome answers 500 internal, which must stay at 0).
var errStore = errors.New("cmek: dek store")

// farFuture is the M1 stand-in for a lease: every handle stays valid until M2 installs the real lease.
func farFuture() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }

// Manager (M1) is a DEK cache without a lease: one fetch per tenant, then cached; nothing purges.
type Manager struct {
	mu      sync.Mutex
	tenants map[string]*tenant
	order   []*tenant
	sf      singleflight.Group
	ctx     context.Context
	cfg     Config
	log     throttle
}

// New builds a Manager; ctx bounds every fetch.
func New(ctx context.Context, cfg Config) *Manager {
	if cfg.Spawn == nil {
		cfg.Spawn = func(f func()) { go f() }
	}
	m := &Manager{tenants: make(map[string]*tenant, len(cfg.Tenants)), ctx: ctx, cfg: cfg, log: throttle{log: cfg.Logger, last: make(map[string]time.Time)}}
	for i, spec := range cfg.Tenants {
		t := &tenant{idx: i, spec: spec, deks: make(map[string]*dek)}
		m.tenants[spec.ID] = t
		m.order = append(m.order, t)
	}
	return m
}

// EncryptKey returns a handle on the tenant's active DEK, generating one through the KMS on the cold path (singleflight per tenant).
func (m *Manager) EncryptKey(ctx context.Context, id string) (Handle, error) {
	m.mu.Lock()
	t := m.tenants[id]
	if t == nil {
		m.mu.Unlock()
		return Handle{}, ErrKeyUnavailable
	}
	if t.active != nil && t.active.key != nil {
		h := Handle{DEKID: t.active.id, ValidUntil: farFuture(), key: *t.active.key}
		t.active.msgs++
		m.mu.Unlock()
		return h, nil
	}
	m.mu.Unlock()
	_, err, _ := m.sf.Do(id+"/generate", func() (any, error) { return nil, m.generate(t) })
	if err != nil {
		if errors.Is(err, errStore) {
			return Handle{}, err
		}
		return Handle{}, ErrKeyUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.active == nil || t.active.key == nil {
		return Handle{}, ErrKeyUnavailable
	}
	t.active.msgs++
	return Handle{DEKID: t.active.id, ValidUntil: farFuture(), key: *t.active.key}, nil
}

// generate calls GenerateDataKey with a detached deadline, persists the wrapped form, then installs the DEK as active.
func (m *Manager) generate(t *tenant) error {
	m.mu.Lock()
	if t.active != nil && t.active.key != nil {
		m.mu.Unlock()
		return nil
	}
	t.seq++
	dekID := t.spec.ID + "/" + strconv.Itoa(t.seq)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.KMSTimeout)
	defer cancel()
	start := m.cfg.Clock.Now()
	dk, err := m.cfg.Keys.GenerateDataKey(ctx, t.spec.KEKID)
	latency := m.cfg.Clock.Now().Sub(start)
	if err != nil {
		m.cfg.Audit.Audit(Audit{At: m.cfg.Clock.Now(), Tenant: t.spec.ID, Op: "generate", Outcome: "error", Detail: err.Error(), Class: Transient, Latency: latency})
		m.log.Error("kms generate failed", "tenant", t.spec.ID, "kek", t.spec.KEKID, "latency", latency, "err", err)
		return err
	}
	if err := m.cfg.Store.PutDEK(m.ctx, WrappedDEK{ID: dekID, Tenant: t.spec.ID, KEKID: t.spec.KEKID, KEKVersion: dk.KEKVersion, Wrapped: dk.Wrapped, CreatedAt: m.cfg.Clock.Now()}); err != nil {
		m.log.Error("dek store write failed", "tenant", t.spec.ID, "dek", dekID, "err", err)
		return fmt.Errorf("%w: %v", errStore, err)
	}
	key := dk.Plaintext
	m.mu.Lock()
	d := &dek{id: dekID, wrapped: dk.Wrapped, kekVersion: dk.KEKVersion, key: &key, createdAt: m.cfg.Clock.Now()}
	t.deks[dekID] = d
	t.active = d
	m.mu.Unlock()
	m.cfg.Audit.Audit(Audit{At: m.cfg.Clock.Now(), Tenant: t.spec.ID, Op: "generate", Outcome: "ok", KEKVersion: dk.KEKVersion, Class: OK, Latency: latency})
	return nil
}

// DecryptKey returns a copy of the named DEK; ErrPoison for a dek_id the tenant does not own; it never blocks and never calls a KMS.
func (m *Manager) DecryptKey(id, dekID string) (Handle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenants[id]
	if t == nil {
		return Handle{}, ErrPoison
	}
	d := t.deks[dekID]
	if d == nil {
		return Handle{}, ErrPoison
	}
	if d.key == nil {
		return Handle{}, ErrDEKCold
	}
	return Handle{DEKID: dekID, ValidUntil: farFuture(), key: *d.key}, nil
}

// Hot reports whether the scheduler may dispatch the tenant now (M1: always).
func (m *Manager) Hot(id string) bool { return true }

// Warm records that a worker needs a purged DEK (M1: no-op; nothing purges).
func (m *Manager) Warm(id, dekID string) {}

// Tick runs lease expiry, purges and probes (M1: no-op); the World sweep calls it every SweepInterval.
func (m *Manager) Tick(now time.Time) {}

// States copies each tenant's State into dst in grid order (M1: every tenant is Active).
func (m *Manager) States(dst []State) {
	for i := range dst {
		dst[i] = Active
	}
}
