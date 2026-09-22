// Owner: Claude (reviewed by Ben)
package cmek

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prabenzo/cmek/internal/kms"
)

// errBusy is the per-tenant in-flight cap: unclassified, never audited, never fed to backoff.
var errBusy = errors.New("cmek: tenant inflight cap")

// errStore marks a DEKStore failure: not a KMS outcome, so no audit line, no state change, and EncryptKey returns it
// unclassified (world.Outcome answers 500 internal, which must stay at 0).
var errStore = errors.New("cmek: dek store")

// result is one KMS outcome as apply consumes it.
type result struct {
	sentAt  time.Time
	class   Class
	err     error
	key     [32]byte
	dk      kms.DataKey
	latency time.Duration
}

// call is the only function that touches kms.KMS: tenant cap → provider semaphore raced against a KMSTimeout
// context → sentAt → Unwrap | GenerateDataKey → release → classify. It runs with t.mu released and emits no audit.
func (m *Manager) call(t *tenant, op string, d *dek) result {
	if m.cfg.TenantInflight > 0 {
		t.mu.Lock()
		if t.inflight >= m.cfg.TenantInflight {
			t.mu.Unlock()
			return result{err: errBusy}
		}
		t.inflight++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.inflight--
			t.mu.Unlock()
		}()
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.KMSTimeout)
	defer cancel()
	sem := m.provSem[t.spec.Provider]
	select {
	case sem <- struct{}{}:
	case <-ctx.Done(): // the bulkhead wait exhausted the deadline: a transient timeout, never a deny
		return result{sentAt: m.cfg.Clock.Now(), class: Transient, err: fmt.Errorf("bulkhead %s: %w", t.spec.Provider, ctx.Err())}
	}
	m.inflight[t.spec.Provider].Add(1)
	var r result
	r.sentAt = m.cfg.Clock.Now() // read after the semaphore, before the call: lease validity runs from here
	if op == "generate" {
		r.dk, r.err = m.cfg.Keys.GenerateDataKey(ctx, t.spec.KEKID)
	} else {
		r.key, r.err = m.cfg.Keys.Unwrap(ctx, t.spec.KEKID, d.kekVersion, d.wrapped)
	}
	r.latency = m.cfg.Clock.Now().Sub(r.sentAt)
	m.inflight[t.spec.Provider].Add(-1)
	<-sem
	r.class = Classify(r.err)
	return r
}

// probe renews the tenant's authorization: Unwrap of the active DEK (singleflight "<t>/active") or, when the tenant
// has no DEK at all, a GenerateDataKey ("<t>/generate"); then apply. [SC-F2]
func (m *Manager) probe(t *tenant) {
	t.mu.Lock()
	d := t.active
	t.mu.Unlock()
	if d == nil {
		m.sf.Do(t.spec.ID+"/generate", func() (any, error) { _, r := m.generate(t); return nil, r.err })
		return
	}
	m.sf.Do(t.spec.ID+"/active", func() (any, error) {
		r := m.call(t, "unwrap", d)
		m.apply(t, "unwrap", d, r)
		return nil, r.err
	})
}

// warm unwraps one non-active DEK a worker asked for (singleflight "<t>/<dekID>"), then apply.
func (m *Manager) warm(t *tenant, d *dek) {
	m.sf.Do(t.spec.ID+"/"+d.id, func() (any, error) {
		r := m.call(t, "unwrap", d)
		m.apply(t, "warm", d, r)
		return nil, r.err
	})
}

// generate calls GenerateDataKey, allocates "<t>/<seq+1>" on success, persists the wrapped form BEFORE apply, then
// installs the DEK as active through apply. A PutDEK error is not a KMS outcome: no state change, no audit, no
// backoff (like errBusy); the store error goes back unclassified and a Tick-spawned probe simply retries next sweep.
func (m *Manager) generate(t *tenant) (*dek, result) {
	r := m.call(t, "generate", nil)
	if r.err != nil {
		m.apply(t, "generate", nil, r)
		return nil, r
	}
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	t.seq++
	d := &dek{id: t.spec.ID + "/" + strconv.Itoa(t.seq), wrapped: r.dk.Wrapped, kekVersion: r.dk.KEKVersion, createdAt: now}
	t.mu.Unlock()
	if err := m.cfg.Store.PutDEK(m.ctx, WrappedDEK{ID: d.id, Tenant: t.spec.ID, KEKID: t.spec.KEKID, KEKVersion: d.kekVersion, Wrapped: d.wrapped, CreatedAt: now}); err != nil {
		m.log.Error("dek store write failed", "tenant", t.spec.ID, "dek", d.id, "err", err)
		r.err = fmt.Errorf("%w: %v", errStore, err)
		m.apply(t, "generate", d, r)
		return nil, r
	}
	m.apply(t, "generate", d, r)
	return d, r
}

// apply is the only state mutator, always under t.mu. Rows exactly as ARCH › (c) apply table: OK / Transient / Deny,
// plus errBusy and a store error (no change, no audit). It clears probing (probe and warm completions), writes
// states[idx], and emits the audit lines: the unwrap/generate line first, then a state line if the state changed.
// So Auditor.Audit is always called under t.mu and the order is deterministic (PutDEK → apply → audits).
func (m *Manager) apply(t *tenant, op string, d *dek, r result) {
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.probing = false
	if errors.Is(r.err, errBusy) || errors.Is(r.err, errStore) {
		if op == "warm" && d != nil {
			t.pending[d.id] = now.Add(m.cfg.SweepInterval)
		}
		return
	}
	auditOp := op
	if op == "warm" {
		auditOp = "unwrap"
	}
	detail := ""
	if d != nil && op == "warm" {
		detail = "warm " + d.id
	}
	switch r.class {
	case OK:
		if r.sentAt.Before(t.deniedAt) {
			m.audit(Audit{At: now, Tenant: t.spec.ID, Op: auditOp, Outcome: "stale ok ignored", Detail: detail, Class: OK, Latency: r.latency})
			return // an OK checked before the revoke must not un-park the tenant [BB-04 partial]
		}
		m.audit(Audit{At: now, Tenant: t.spec.ID, Op: auditOp, Outcome: "ok", Detail: detail, KEKVersion: d.kekVersion, Class: OK, Latency: r.latency})
		t.lease.Renew(r.sentAt)
		key := r.key
		if op == "generate" {
			key = r.dk.Plaintext
			t.deks[d.id] = d
			t.active = d
		}
		d.key = &key
		d.hotSince = now
		t.attempt = 0
		delete(t.pending, d.id)
		if from := t.state; from != Active {
			m.setState(t, Active, now, "authorization renewed", 0)
			if from == Revoked && m.cfg.Logger != nil {
				m.cfg.Logger.Info("tenant restored", "tenant", t.spec.ID) // the end of a revocation episode is worth a line
			}
		}
	case Deny:
		m.audit(Audit{At: now, Tenant: t.spec.ID, Op: auditOp, Outcome: "denied", Detail: r.err.Error(), Class: Deny, Latency: r.latency})
		purged := t.purge()
		t.deniedAt = now                              // every deny: the stale-OK guard needs the latest
		t.lease.SentAt = time.Time{}                  // no lease (the TTLs are configuration and stay)
		t.nextProbeAt = now.Add(m.cfg.RevokedReprobe) // fixed period, no jitter; attempt untouched
		if t.state != Revoked {
			m.setState(t, Revoked, now, fmt.Sprintf("%d DEKs purged", purged), purged)
		}
	default: // Transient (and Poison, which no KMS call produces: treated as an unanswered call)
		m.audit(Audit{At: now, Tenant: t.spec.ID, Op: auditOp, Outcome: "error", Detail: r.err.Error(), Class: r.class, Latency: r.latency})
		t.attempt++
		t.nextProbeAt = now.Add(m.backoff(t.attempt))
		if op == "warm" && d != nil {
			t.pending[d.id] = t.nextProbeAt
		}
		usable := t.lease.Usable(now)
		switch {
		case t.state == Active && usable:
			m.setState(t, RidingThrough, now, "renewal failed", 0)
		case t.state == Active && !usable: // a cold tenant: the two spec edges compose in one call (Q3)
			purged := t.purge()
			m.setState(t, KeyUnavailable, now, "cold fetch failed", purged)
		case t.state == RidingThrough && !usable:
			purged := t.purge()
			m.setState(t, KeyUnavailable, now, fmt.Sprintf("lease expired, %d DEKs purged", purged), purged)
		}
	}
}

// setState changes the tenant's state under t.mu, publishes it to the lock-free grid array and audits the transition.
func (m *Manager) setState(t *tenant, to State, now time.Time, why string, purged int) {
	from := t.state
	t.state = to
	m.states[t.idx].Store(uint32(to))
	m.audit(Audit{At: now, Tenant: t.spec.ID, Op: "state", Outcome: from.String() + "→" + to.String(), Detail: why, From: from, To: to, Purged: purged})
}

func (m *Manager) audit(e Audit) {
	if m.cfg.Audit != nil {
		m.cfg.Audit.Audit(e)
	}
}

// backoff wraps Backoff with the configured bounds and the injected jitter.
func (m *Manager) backoff(n int) time.Duration {
	u := 0.5
	if m.cfg.Jitter != nil {
		u = m.cfg.Jitter.Float64()
	}
	return Backoff(n, m.cfg.BackoffMin, m.cfg.BackoffMax, m.cfg.BackoffJitter, u)
}
