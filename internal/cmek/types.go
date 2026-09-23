// Owner: Claude (reviewed by Ben)
package cmek

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tink-crypto/tink-go/v2/tink"

	"github.com/prabenzo/cmek/internal/kms"
)

// State is a tenant's key state.
type State uint8

const (
	Active State = iota
	RidingThrough
	KeyUnavailable
	Revoked
)

// String returns ACTIVE, RIDING_THROUGH, KEY_UNAVAILABLE or REVOKED.
func (s State) String() string {
	switch s {
	case Active:
		return "ACTIVE"
	case RidingThrough:
		return "RIDING_THROUGH"
	case KeyUnavailable:
		return "KEY_UNAVAILABLE"
	case Revoked:
		return "REVOKED"
	}
	return "UNKNOWN"
}

// Class sorts a KMS outcome into the four treatments of the down-versus-revoked table.
type Class uint8

const (
	OK Class = iota
	Transient
	Deny
	Poison
)

// String names the class.
func (c Class) String() string {
	switch c {
	case OK:
		return "ok"
	case Transient:
		return "transient"
	case Deny:
		return "deny"
	case Poison:
		return "poison"
	}
	return "unknown"
}

// Envelope is one message's ciphertext plus what is needed to open it: the DEK id and Tink's AES-256-GCM output
// (IV || ciphertext || tag). AAD is derived, never stored.
type Envelope struct {
	DEKID      string
	Ciphertext []byte
}

// Handle is one usable DEK primitive until ValidUntil; Seal never blocks and never touches a lock. The primitive is
// shared with the cache (Tink primitives are immutable and safe for concurrent use); Zero drops this reference.
type Handle struct {
	DEKID      string
	ValidUntil time.Time
	prim       tink.AEAD
}

// Zero drops the handle's reference to the key. Tink holds key material in ordinary Go memory and offers no
// zeroization: plaintext DEKs are dropped on purge and collected, never wiped in place.
func (h *Handle) Zero() { h.prim = nil }

// WrappedDEK is what DEKStore persists: wrapped bytes only.
type WrappedDEK struct {
	ID, Tenant, KEKID string
	KEKVersion        int
	Wrapped           []byte
	CreatedAt         time.Time
}

// Audit is one line of the per-tenant audit log and the source of the timeline.
type Audit struct {
	At                          time.Time
	Tenant, Op, Outcome, Detail string
	KEKVersion                  int
	Class                       Class
	From, To                    State
	Latency                     time.Duration
	Purged                      int
}

// TenantInfo is the read model for GET /v1/tenants/{id} and grid hover.
type TenantInfo struct {
	State                                    State
	LeaseAge, LeaseRemaining, NextProbeIn    time.Duration
	ActiveDEK                                string
	DEKs, HotDEKs, Pending, Attempt, Waiters int
	Probing                                  bool
}

// TenantSpec is the immutable per-tenant wiring, given in grid order.
type TenantSpec struct {
	ID, Provider, KEKID string
}

var ErrKeyUnavailable = errors.New("cmek: key unavailable") // ingest: parked, cold fetch failed transiently, or bulkhead/waiter cap full
var ErrKeyRevoked = errors.New("cmek: key revoked")         // ingest or decrypt: authoritative deny
var ErrLeaseExpired = errors.New("cmek: lease expired")     // decrypt: no usable lease now; seal: stale handle
var ErrDEKCold = errors.New("cmek: dek not cached")         // decrypt: lease usable, plaintext DEK missing
var ErrPoison = errors.New("cmek: authentication failed")   // open: GCM tag mismatch; decrypt: unknown or foreign dek_id

// DEKStore persists wrapped DEKs; implemented by *queue.Store.
type DEKStore interface {
	PutDEK(ctx context.Context, d WrappedDEK) error
}

// Auditor receives every KMS call, purge and state change; implemented by world's bridge to metrics.
type Auditor interface{ Audit(e Audit) }

// Clock is the only time source the core reads.
type Clock interface{ Now() time.Time }

// Jitter is the only randomness the core reads (backoff); Tink draws the IVs and the DEKs from crypto/rand.
type Jitter interface{ Float64() float64 }

// Config carries the lease parameters and injected dependencies; zero Spawn means go f().
type Config struct {
	Tenants   []TenantSpec
	Providers []string
	Keys      kms.KMS
	Store     DEKStore
	Audit     Auditor
	Clock     Clock
	Jitter    Jitter
	Spawn     func(func()) // tests pass func(f func()) { f() }
	Logger    *slog.Logger // optional; every KMS and store error is logged (throttled to one line per second per message)

	Lease, SoftTTL, EarlyExpiry, KMSTimeout, BackoffMin, BackoffMax, RevokedReprobe, DEKMaxAge, SweepInterval time.Duration
	BackoffJitter                                                                                             float64
	DEKMaxMessages, ProviderInflight, TenantInflight, IngestWaiters                                           int
}
