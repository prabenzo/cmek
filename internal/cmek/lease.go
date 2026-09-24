// Owner: Claude (reviewed by Ben)
package cmek

import (
	"math"
	"sync"
	"time"

	"github.com/tink-crypto/tink-go/v2/tink"
)

// Lease is the window in which cached DEKs may be used; the zero value is "no lease".
// Validity runs from the instant the successful KMS call was sent, never from its reply.
type Lease struct {
	SentAt              time.Time
	TTL, SoftTTL, Early time.Duration
}

// Renew moves SentAt forward only; returns false when sentAt is not later than SentAt.
func (l *Lease) Renew(sentAt time.Time) bool {
	if !sentAt.After(l.SentAt) {
		return false
	}
	l.SentAt = sentAt
	return true
}

// Until is the instant the lease stops being usable: SentAt + TTL − Early. Every ValidUntil a Handle carries
// comes from here, so Seal/Open and Usable can never disagree.
func (l Lease) Until() time.Time { return l.SentAt.Add(l.TTL - l.Early) }

// Usable reports SentAt != 0 && now < Until() (29 s of use from the send instant).
func (l Lease) Usable(now time.Time) bool {
	return !l.SentAt.IsZero() && now.Before(l.Until())
}

// SoftDue reports now ≥ SentAt + SoftTTL (lazy renewal may start).
func (l Lease) SoftDue(now time.Time) bool {
	return !now.Before(l.SentAt.Add(l.SoftTTL))
}

// Remaining returns SentAt + TTL − Early − now (≤ 0 when lapsed or absent).
func (l Lease) Remaining(now time.Time) time.Duration {
	if l.SentAt.IsZero() {
		return 0
	}
	return l.Until().Sub(now)
}

// Backoff returns min(min×2^(n−1), max) × (1 + jitter × (2u − 1)) for attempt n ≥ 1 and u ∈ [0,1).
func Backoff(n int, min, max time.Duration, jitter, u float64) time.Duration {
	if n < 1 {
		n = 1
	}
	base := float64(min) * math.Pow(2, float64(n-1))
	if base > float64(max) {
		base = float64(max)
	}
	return time.Duration(base * (1 + jitter*(2*u-1)))
}

// dek is one DEK: wrapped bytes (the keyset encrypted under the KEK) forever, the primitive only while hot.
// prim == nil means cold.
type dek struct {
	id         string
	wrapped    []byte
	kekVersion int
	prim       tink.AEAD
	msgs       int
	createdAt  time.Time
	hotSince   time.Time
}

// tenant is one tenant and its own lock: mu protects every field below it (Decision: Manager locking → per-tenant
// mutexes). A method locks at most one tenant at a time and never holds mu across a KMS call, sf.Do, PutDEK or Spawn.
type tenant struct {
	mu sync.Mutex

	idx  int
	spec TenantSpec
	seq  int // next DEK id is "<tenant>/<seq+1>"

	state State
	lease Lease

	deks   map[string]*dek
	active *dek

	attempt     int       // consecutive transient failures; reset by any success; a deny never touches it
	nextProbeAt time.Time // Tick probes at or after this instant while not ACTIVE
	deniedAt    time.Time // the latest deny; an OK sent before it is stale
	probing     bool      // one probe or warm in flight

	pending map[string]time.Time // dekID → dueAt: a worker needs this cold DEK [SC-F5]

	waiters, inflight int // IngestWaiters cap; TenantInflight cap (0 = none)

	fetch Fetch // where this tenant's KMS calls run (FetchAsync unless a demo switched it)
}

// Fetch is where a tenant's KMS calls run: the design that ships, or one of the demo designs the Slow KMS and
// no-cache cards switch to (KEYFETCH.md, NOCACHE.md).
type Fetch uint8

const (
	FetchAsync       Fetch = iota // renewals and probes are kicked into the background; workers never wait on the KMS
	FetchSync                     // cache, lease and bulkheads kept; the worker renews a due lease and probes a parked tenant inline
	FetchPassThrough              // no lease, no cache; bulkheads kept: every seal and delivery is one KMS call
	FetchNaive                    // pass-through with no bulkheads either
)

// direct reports a mode without the cache (pass-through or naive).
func (t *tenant) direct() bool { return t.fetch == FetchPassThrough || t.fetch == FetchNaive }

// hot counts the DEKs whose plaintext is cached.
func (t *tenant) hot() int {
	n := 0
	for _, d := range t.deks {
		if d.prim != nil {
			n++
		}
	}
	return n
}

// purge drops every plaintext primitive, clears pending, keeps wrapped bytes; returns how many were hot.
func (t *tenant) purge() int {
	n := 0
	for _, d := range t.deks {
		if d.prim != nil {
			d.prim = nil
			n++
		}
	}
	for k := range t.pending {
		delete(t.pending, k)
	}
	return n
}
