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

// renew runs one authorization call for the tenant behind singleflight: a GenerateDataKey when the tenant has no
// DEK (or, on the cold path, an exhausted active one), else an Unwrap of the active DEK; then apply. Inside the
// flight it rechecks under t.mu whether the call is still needed, because a concurrent flight may have landed
// between the caller's decision and this one: two cold callers never generate two DEKs, and two probes never
// unwrap twice for one need. cold means the caller needs a usable handle now; a probe needs a renewal. [SC-F2]
func (m *Manager) renew(t *tenant, cold bool) error {
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	d := t.active
	gen := d == nil || (cold && t.state == Active && m.exhausted(d, now))
	t.mu.Unlock()
	key := t.spec.ID + "/active"
	if gen {
		key = t.spec.ID + "/generate"
	}
	_, err, _ := m.sf.Do(key, func() (any, error) {
		if m.satisfied(t, cold) {
			return nil, nil
		}
		if gen {
			_, r := m.generate(t)
			return nil, r.err
		}
		r := m.call(t, "unwrap", d)
		m.apply(t, "unwrap", d, r)
		return nil, r.err
	})
	return err
}

// satisfied reports, under t.mu, that the need behind a renew has already been met: a cold caller can seal now;
// a probe's renewal has already happened (an ACTIVE tenant with a usable lease that is not yet soft-due). A tenant
// in any other state needs a successful call to heal, so its probe is never skipped.
func (m *Manager) satisfied(t *tenant, cold bool) bool {
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.active
	fresh := d != nil && d.key != nil && t.lease.Usable(now) && !(t.state == Active && m.exhausted(d, now))
	if cold {
		return fresh
	}
	return t.state == Active && fresh && !t.lease.SoftDue(now)
}

// probe is the kicked renewal (EncryptKey hot path, Hot, Tick). probing is cleared here, in the kicker's own
// goroutine after sf.Do has returned, never inside the flight: a probe that joined an already-finished flight
// (singleflight drops its key only after the function returns) would otherwise leave the flag set forever.
func (m *Manager) probe(t *tenant) {
	m.renew(t, false)
	m.clearProbing(t)
}

// warm unwraps one non-active DEK a worker asked for (singleflight "<t>/<dekID>"), then apply; probing is
// cleared after the flight as in probe. A DEK that another flight has already warmed is not unwrapped again.
func (m *Manager) warm(t *tenant, d *dek) {
	m.sf.Do(t.spec.ID+"/"+d.id, func() (any, error) {
		t.mu.Lock()
		hot := d.key != nil
		if hot {
			delete(t.pending, d.id)
		}
		t.mu.Unlock()
		if hot {
			return nil, nil
		}
		r := m.call(t, "unwrap", d)
		m.apply(t, "warm", d, r)
		return nil, r.err
	})
	m.clearProbing(t)
}

func (m *Manager) clearProbing(t *tenant) {
	t.mu.Lock()
	t.probing = false
	t.mu.Unlock()
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
// plus errBusy and a store error (no change, no audit). It writes states[idx] and emits the audit lines: the
// unwrap/generate line first, then a state line if the state changed. So Auditor.Audit is always called under
// t.mu and the order is deterministic (PutDEK → apply → audits). Every failed call is also logged, one line per
// second per message, so an outage is visible in the service log and not only in the audit ring.
func (m *Manager) apply(t *tenant, op string, d *dek, r result) {
	now := m.cfg.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
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
		m.log.Error("kms call denied", "tenant", t.spec.ID, "op", auditOp, "state", t.state.String(), "err", r.err)
		purged := t.purge()
		t.deniedAt = now                              // every deny: the stale-OK guard needs the latest
		t.lease.SentAt = time.Time{}                  // no lease (the TTLs are configuration and stay)
		t.nextProbeAt = now.Add(m.cfg.RevokedReprobe) // fixed period, no jitter; attempt untouched
		if t.state != Revoked {
			m.setState(t, Revoked, now, fmtPurged("key revoked", purged), purged)
		}
	default: // Transient (and Poison, which no KMS call produces: treated as an unanswered call)
		m.audit(Audit{At: now, Tenant: t.spec.ID, Op: auditOp, Outcome: "error", Detail: r.err.Error(), Class: r.class, Latency: r.latency})
		m.log.Error("kms call failed", "tenant", t.spec.ID, "op", auditOp, "class", r.class.String(), "state", t.state.String(), "attempt", t.attempt+1, "err", r.err)
		t.attempt++
		t.nextProbeAt = now.Add(m.backoff(t.attempt))
		for id := range t.pending { // every pending warm waits for the same next slot: one KMS call per interval [SC-F5]
			t.pending[id] = t.nextProbeAt
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
			m.setState(t, KeyUnavailable, now, fmtPurged("lease expired", purged), purged)
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
