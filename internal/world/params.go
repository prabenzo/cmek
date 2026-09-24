// Owner: Claude
package world

import "time"

// Params holds every time constant and load setting; nothing is a constant buried in a package. M1 shape; later milestones add fields.
type Params struct {
	// Population
	Tenants   int      // 1000
	Providers []string // {"aws","gcp","azure"}; grid index i belongs to Providers[i*len/Tenants] (contiguous bands)
	Seed      int64    // 1; seeds the World's *rand.Rand; DEKs, KEKs and IVs come from crypto/rand inside Tink

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
	TwoClassSched     bool          // true interleave within-share and over-share turns (M3 Decision)
	SchedLightTurns   int           // 32   within-share turns per over-share turn (ARCH: 4; unstable below ≈ 18, M3 Decision)
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

	// Admission
	TenantRate       float64    // 100 events/s
	TenantBurst      int        // 200
	TenantBacklogCap int        // 500
	GlobalBacklogCap int        // 20000
	FairShare        bool       // true; false = plain global cap (M3 cut line)
	RetryAfter       RetryAfter // fixed header values, all of them

	// Scenarios (M3: the two surges and the tail bounds; M4 adds the fault scenarios)
	SurgeTenantRank        int           // 5   a top-20 tenant
	TenantSurgeMult        float64       // 100
	TenantSurgeFor         time.Duration // 60s
	GlobalSurgeMult        float64       // 5
	GlobalSurgeFor         time.Duration // 90s (M3 Decision; the spec card says 60 s)
	GlobalSurgeAffectedTop int           // 150 heaviest ranks form the declared affected set
	ScenarioTailMin        time.Duration // 15s observation after the fault clears, at least this long (M4 reads it)
	ScenarioTailMax        time.Duration // 90s tail ends earlier when every target is ACTIVE and affected backlog is drained

	// Fault scenarios (M4)
	OutageBlip       time.Duration // 10s  provider_blip fast-fail
	OutageLong       time.Duration // 60s  provider_outage fast-fail
	OutageProvider   string        // "gcp"
	SlowProvider     string        // "azure"
	SlowP50          time.Duration // 400ms slow_kms latency p50
	SlowP99          time.Duration // 3s    slow_kms latency p99 (against the 500 ms timeout)
	SlowFor          time.Duration // 60s
	RevokeTenantRank int           // 3    a high-traffic tenant

	// No key cache (NOCACHE.md): two runs at a realistic KMS latency on every provider
	NoCacheProvider   string        // "gcp" the band that loses the cache in the one-band run
	NoCacheFor        time.Duration // 30s  cache off before the blip
	NoCacheBlip       time.Duration // 10s  the provider's fast-fail inside the run
	NoCacheAfter      time.Duration // 15s  cache still off after the blip clears
	NoCacheKMSP50     time.Duration // 20ms latency every provider gets for the run
	NoCacheKMSP99     time.Duration // 80ms
	NoCacheSurgeLead  time.Duration // 15s  everyone without the cache before the surge
	NoCacheSurgeFor   time.Duration // 60s  the global surge (GlobalSurgeMult) without the cache
	NoCacheSurgeAfter time.Duration // 15s  cache still off after the surge ends
	NaiveFor          time.Duration // 30s  slow_kms_naive: the naive design for everyone with the slow provider (NAIVE.md)
	NaiveLeasedFor    time.Duration // 30s  then leases, cache and bulkheads back with the provider still slow

	// Charts, p99 and the invariant thresholds (M4 measures; M5 judges)
	ChartWindow          time.Duration // 2m   ring the page keeps per series
	P99Window            int           // 10   ticks (5 s) summed for p99
	BaselineTicks        int           // 20   ticks (10 s) before scenario start; baseline = mean of the tick p99s
	L1Ratio              float64       // 1.25
	L1Floor              time.Duration // 25ms L1 compares against L1Ratio × max(baseline, L1Floor) (M4 Decision)
	L1Grace              time.Duration // 7s   after scenario start before L1 is judged: P99Window × SnapshotInterval + 2 s, so the opening burst has left the window (M5)
	L1For                time.Duration // 3s   the healthy p99 must stay over its limit this long before L1 turns red; 0 judges every pass (M5)
	L4CapacityFactor     float64       // 0.9  delivered/s must stay ≥ this × measured capacity while backlogged
	L4Settle             time.Duration // 7s   backlog must exceed Workers×ClaimBatch this long before L4 is judged: P99Window × SnapshotInterval + 2 s, so the delivered window holds no pre-surge seconds (M5)
	TimelineAggregateMin int           // 3    same (provider, from, to) transitions in one tick collapse to one line

	// Checker (M5)
	CheckInterval  time.Duration // 1s   one checker pass
	CanaryFullScan time.Duration // 5s   S1's full scan of the stored rows
	Lights         []string      // {"S1","S2","S3","S4","L1","L4"}: which lights the checker judges; strike an id to grey it
	Capacity       float64       // 620  the delivered plateau M4 measured (L4's threshold is L4CapacityFactor × it); 0 → Workers / mean sink latency

	// Queue retention (M5)
	Retention      time.Duration // 10m  ready rows older than this expire
	ExpireInterval time.Duration // 5s   sweep period for Expire
	ExpireLimit    int           // 1000 rows per Expire call

	// Stream (M5)
	StreamMaxAge       time.Duration // 55m  the handler ends a stream with event: reconnect before Cloud Run's 60-minute cut
	StreamWriteTimeout time.Duration // 5s   per-frame write deadline

	// Lifecycle
	IdleRebuild time.Duration // 10s  no viewers longer than this → next connection builds a fresh World (M5)
	StopTimeout time.Duration // 2s   Stop waits at most this long for goroutines and released handlers
}

// Judges reports whether the checker judges the named light.
func (p Params) Judges(light string) bool {
	for _, l := range p.Lights {
		if l == light {
			return true
		}
	}
	return false
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
		TwoClassSched: true, SchedLightTurns: 32,
		SyncMode: "normal", WALAutocheckpoint: 4000,
		SinkLatencyMin: 5 * time.Millisecond, SinkLatencyMax: 20 * time.Millisecond, SinkRing: 1024,
		BaseRate: 300, ZipfExponent: 1, PayloadBytes: 1024, CanaryPrefix: "PLAINTEXT-CANARY-",
		SnapshotInterval: 500 * time.Millisecond, AuditRing: 16, TimelineRing: 200, SnapshotEvents: 20, ViewerQueue: 1,
		TenantRate: 100, TenantBurst: 200, TenantBacklogCap: 500, GlobalBacklogCap: 20000, FairShare: true,
		RetryAfter:      RetryAfter{RateLimited: 1, BacklogFull: 5, Overloaded: 5, KeyUnavailable: 1},
		SurgeTenantRank: 5, TenantSurgeMult: 100, TenantSurgeFor: 60 * time.Second, GlobalSurgeMult: 5, GlobalSurgeFor: 90 * time.Second,
		GlobalSurgeAffectedTop: 150, ScenarioTailMin: 15 * time.Second, ScenarioTailMax: 90 * time.Second,
		OutageBlip: 10 * time.Second, OutageLong: 60 * time.Second, OutageProvider: "gcp", SlowProvider: "azure",
		SlowP50: 400 * time.Millisecond, SlowP99: 3 * time.Second, SlowFor: 60 * time.Second, RevokeTenantRank: 3,
		NoCacheProvider: "gcp", NoCacheFor: 30 * time.Second, NoCacheBlip: 10 * time.Second, NoCacheAfter: 15 * time.Second,
		NoCacheKMSP50: 20 * time.Millisecond, NoCacheKMSP99: 80 * time.Millisecond,
		NoCacheSurgeLead: 15 * time.Second, NoCacheSurgeFor: 60 * time.Second, NoCacheSurgeAfter: 15 * time.Second,
		NaiveFor: 30 * time.Second, NaiveLeasedFor: 30 * time.Second,
		ChartWindow: 2 * time.Minute, P99Window: 10, BaselineTicks: 20, L1Ratio: 1.25, L1Floor: 25 * time.Millisecond, L1Grace: 7 * time.Second, L1For: 3 * time.Second,
		L4CapacityFactor: 0.9, L4Settle: 7 * time.Second, TimelineAggregateMin: 3,
		CheckInterval: time.Second, CanaryFullScan: 5 * time.Second, Lights: []string{"S1", "S2", "S3", "S4", "L1", "L4"}, Capacity: 620,
		Retention: 10 * time.Minute, ExpireInterval: 5 * time.Second, ExpireLimit: 1000,
		StreamMaxAge: 55 * time.Minute, StreamWriteTimeout: 5 * time.Second,
		IdleRebuild: 10 * time.Second, StopTimeout: 2 * time.Second,
	}
}

// Small returns Demo with Tenants=20, Workers=2, BaseRate=20 for tests.
func Small() Params {
	p := Demo()
	p.Tenants, p.Workers, p.BaseRate = 20, 2, 20
	return p
}
