// Owner: Claude
package world

import "time"

// Params holds every time constant and load setting; nothing is a constant buried in a package. M1 shape; later milestones add fields.
type Params struct {
	// Population
	Tenants   int      // 1000
	Providers []string // {"aws","gcp","azure"}; grid index i belongs to Providers[i*len/Tenants] (contiguous bands)
	Seed      int64    // 1; seeds the World's *rand.Rand; nonces and KEKs use crypto/rand

	// Key lease (cmek)
	Lease            time.Duration // 30s   hard TTL: revocation bound and max ride-through
	SoftTTL          time.Duration // 15s   lease age at which lazy background renewal starts
	EarlyExpiry      time.Duration // 1s    lease expires locally this much before Lease
	KMSTimeout       time.Duration // 500ms per-call deadline, includes provider-bulkhead wait
	BackoffMin       time.Duration // 500ms first transient retry
	BackoffMax       time.Duration // 8s    cap
	BackoffJitter    float64       // 0.5   ±50%
	RevokedReprobe   time.Duration // 5s    fixed period while REVOKED
	DEKMaxMessages   int           // 10000 rotate the active DEK after this many seals
	DEKMaxAge        time.Duration // 10m   rotate the active DEK after this age; non-active plaintext dropped at DEKMaxAge + Lease
	ProviderInflight int           // 32    bulkhead per provider
	TenantInflight   int           // 2     bulkhead per tenant; 0 = no per-tenant cap (M2 cut line)
	IngestWaiters    int           // 4     cold-path waiters per tenant, then fast 503
	SweepInterval    time.Duration // 250ms Manager.Tick period (expiry, purge, probes, warm retries)

	// Queue
	Workers           int           // 8
	ClaimBatch        int           // 8    max rows claimed per tenant per turn
	ClaimTimeout      time.Duration // 30s  claimed_until horizon (message claim, not the key lease)
	ReclaimInterval   time.Duration // 1s   sweep returning timed-out claims to ready
	IdlePoll          time.Duration // 20ms worker wait when Next finds nothing
	DBDir             string        // ""   → os.TempDir(); the file is <DBDir>/killswitch-<pid>-<ID>.db
	SyncMode          string        // "normal"; env KS_SYNC
	WALAutocheckpoint int           // 4000; env KS_WAL_AUTOCHECKPOINT

	// Sink (delivery capacity = Workers / mean latency ≈ 640/s)
	SinkLatencyMin time.Duration // 5ms
	SinkLatencyMax time.Duration // 20ms
	SinkRing       int           // 1024 deliveredAt timestamps kept per tenant for S3 (M5)

	// Traffic
	BaseRate     float64 // 300 events/s total; env KS_BASE_RATE
	ZipfExponent float64 // 1.0
	PayloadBytes int     // 1024
	CanaryPrefix string  // "PLAINTEXT-CANARY-"

	// Metrics and SSE
	SnapshotInterval time.Duration // 500ms (2 Hz)
	AuditRing        int           // 16 per tenant
	TimelineRing     int           // 200
	SnapshotEvents   int           // 20 most recent timeline entries per snapshot
	ViewerQueue      int           // 1  per-viewer buffered snapshots, drop-on-slow

	// Admission: Retry-After header values (M3 adds the caps)
	RetryAfter RetryAfter

	// Lifecycle
	IdleRebuild time.Duration // 10s  no viewers longer than this → next connection builds a fresh World (M5)
	StopTimeout time.Duration // 2s   Stop waits at most this long for goroutines
}

// RetryAfter holds the fixed Retry-After header values in whole seconds.
type RetryAfter struct {
	RateLimited    int // 1
	BacklogFull    int // 5
	Overloaded     int // 5
	KeyUnavailable int // 1
}

// Demo returns the spec's demo values.
func Demo() Params {
	return Params{
		Tenants: 1000, Providers: []string{"aws", "gcp", "azure"}, Seed: 1,
		Lease: 30 * time.Second, SoftTTL: 15 * time.Second, EarlyExpiry: time.Second, KMSTimeout: 500 * time.Millisecond,
		BackoffMin: 500 * time.Millisecond, BackoffMax: 8 * time.Second, BackoffJitter: 0.5, RevokedReprobe: 5 * time.Second,
		DEKMaxMessages: 10000, DEKMaxAge: 10 * time.Minute, ProviderInflight: 32, TenantInflight: 2, IngestWaiters: 4,
		SweepInterval: 250 * time.Millisecond,
		Workers:       8, ClaimBatch: 8, ClaimTimeout: 30 * time.Second, ReclaimInterval: time.Second, IdlePoll: 20 * time.Millisecond,
		SyncMode: "normal", WALAutocheckpoint: 4000,
		SinkLatencyMin: 5 * time.Millisecond, SinkLatencyMax: 20 * time.Millisecond, SinkRing: 1024,
		BaseRate: 300, ZipfExponent: 1, PayloadBytes: 1024, CanaryPrefix: "PLAINTEXT-CANARY-",
		SnapshotInterval: 500 * time.Millisecond, AuditRing: 16, TimelineRing: 200, SnapshotEvents: 20, ViewerQueue: 1,
		RetryAfter:  RetryAfter{RateLimited: 1, BacklogFull: 5, Overloaded: 5, KeyUnavailable: 1},
		IdleRebuild: 10 * time.Second, StopTimeout: 2 * time.Second,
	}
}

// Small returns Demo with Tenants=20, Workers=2, BaseRate=20 for tests.
func Small() Params {
	p := Demo()
	p.Tenants, p.Workers, p.BaseRate = 20, 2, 20
	return p
}
