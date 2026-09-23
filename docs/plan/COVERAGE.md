# Spec coverage matrix (cross-milestone completeness critic)

Sources: SPEC.md (binding), P0_EXPLAINED.md, HANDOFF.md, DESIGN-REFERENCE.md (canonical), ARCH-NOTES.md (overrides), plan/M0.md–M6.md (revised plans only; drafts ignored).
Updated after the cross-milestone fix pass: every GAP the critic found was closed in the plans (the closing plan and review-log id are named in the row).
Milestone column = the plan whose Files/Order of work delivers the item. "cut line" = the spec's cut for that box; "expected cut" = the plan itself says the cut is the expected path. "cond." = conditional row in a later plan. **GAP** = no plan delivers it on the expected path.

## 1. P0 scope table (SPEC › Scope, expanded per P0_EXPLAINED)

| Item | Spec ref | Milestone | Note |
| --- | --- | --- | --- |
| Ingest API `POST /v1/events`, same function for the load generator | Data path | M1 | `world.Ingest`/`IngestID`; admission inserted M3 |
| Envelope encryption, AES-256-GCM (tink-go, IV embedded), AAD = tenant\|msgID\|dekID | Data path | M1, TINK | `cmek.Seal`/`Open` |
| SQLite queue (deks, messages tables, index, WAL, single writer) | Data path | M1 | schema + Insert/Claim/Ack/Release/Dead; Expire M3 (Ben, unwired) → wired M5; Census/CanaryFull M5 |
| At-least-once via claim timeout (`Reclaim`) | Data path (P0_EXPLAINED binding) | M1 (optional) → M5 (unconditional) | consistent |
| Fair round-robin scheduler (ring, skips parked) | Data path | M1 (plain ring) → M2 (`Gate.Hot`) → M3 (two-class) | |
| Workers (8, never call a KMS) | Data path | M1 → M2 (DecryptKey branches, Warm) | |
| Fake sink (5–20 ms, canary tenant check, delivery record) | Data path | M1 → M5 (deliveredAt ring) | |
| Key lease, soft/hard TTL, validity from send instant, 1 s early | CMEK core | M2 | `Lease` value type, Ben |
| Singleflight | CMEK core | M1 (skeleton `sf.Do`) → M2 | |
| Error classification (OK/Transient/Deny/Poison) | CMEK core | M2 | `classify.go`, Ben |
| Tenant key state machine (4 states, 8 edges) | CMEK core | M2 | `apply` table |
| Backoff probes 0.5→8 s ±50 %, single probe, REVOKED re-probe 5 s | CMEK core | M2 | `Tick` |
| Key fetcher, only KMS caller | CMEK core | M2 | `fetcher.go` |
| Bulkheads 32/provider | CMEK core | M2 | |
| Bulkheads 2/tenant | CMEK core | M2 (cut line, **expected cut** on M1's slip) | M6 cuts table carries |
| Ingest waiter cap 4 → fast 503 | Isolation table | M2 | |
| Tenant parking | CMEK core | M2 | `Hot` false, no worker time |
| Per-tenant token bucket → 429 rate_limited | Admission | M3 | |
| Per-tenant backlog cap 500 → 429 backlog_full | Admission | M3 | |
| Global backlog cap 20,000, fair-share shed heaviest → 503 overloaded | Admission | M3 (cut line → plain cap) | O(1) rule per Q7 |
| 3 KMS providers, real AES wrapping under per-tenant KEKs | Fakes | M1 | |
| 1,000 tenants, Zipf traffic ≈ 300/s, ~1 KB payload with canary | Fakes | M1 | |
| Fault injection: fast-fail outage | Fakes | M2 | |
| Fault injection: lognormal latency (p50, p99) | Fakes | M2 | |
| Fault injection: error rate | Fakes (KMS faults row) | M2 | |
| Revoke and restore (key state enabled/disabled) | Fakes | M2 | |
| Ground truth log (checker-only) | Fakes | M2 (`Truth`) → M5 (read) | |
| Surge controls: per tenant, global | Fakes | M3 | `POST /v1/traffic`, `Surge` |
| Scenario: provider outage (blip 10 s + outage 60 s) | Scenarios | M4 | |
| Scenario: key revocation (+ Restore) | Scenarios | M4 | |
| Scenario: slow KMS | Scenarios | M4 | |
| Scenario: tenant surge | Scenarios | M3 | |
| Scenario: global surge | Scenarios | M3 | 90 s (M3 decision) |
| UI: scenario cards | UI | M4 | copy by Ben |
| UI: tenant grid | UI | M4 | hover/pin = cut line, upside at 3:03 |
| UI: 4 charts | UI | M4 | charts 1–2 base; 3–4 upside at 2:57 (cut line) |
| UI: invariant panel | UI | M4 (markup + `renderPanel`) → M5 (`invariants` key) | M4 ships `renderPanel`; M5 adds only the `invariants` key (M5 CR-1) |
| UI: event timeline | UI | M2 (ring, lines) → M4 (aggregation, page) | |
| UI: reset | UI | M4 (button, disabled) → M5 (`POST /v1/reset`) | M5 removes the `disabled` attribute (M5 CR-1) |
| Live invariant checker | Checks | M5 | S1–S4; L1/L4 expected cut |
| Unit tests: lease | Checks | M2 | `TestLease` |
| Unit tests: classifier | Checks | M2 | `TestClassify` |
| Unit tests: envelope | Checks | M1 (`TestEnvelope`, Ben) | M2 adds two rows only (M2 CR-2) |
| One shared World, no globals | World | M0 (stub, grep) → M1 (`New`, `TestTwoWorlds`) | |
| Shared-system banner | World / UI header | M4 | |
| Pause when nobody is watching | World | M1 (`SetRunning` via viewer callback) | M1 keeps the `r.Context()` select from M0 (M1 CR-2) |
| Auto-reset when idle > 10 s | World | M5 (`Holder.Acquire`) | |

## 2. Endpoints (SPEC › Stack, API and deployment)

| Endpoint | Milestone | Note |
| --- | --- | --- |
| `POST /v1/events` (202/429/503/403, `X-Tenant-ID`, Retry-After) | M1 (202/404/503 key_unavailable/403) → M3 (429 ×2, 503 overloaded) | |
| `GET /v1/stream` (SSE 2 Hz) | M0 (ticker) → M1 (`Subscribe`, retry/id) → M5 (`Acquire`, deadline, `StreamMaxAge`) | |
| `POST /v1/scenarios/{name}/start`, `/stop` | M3 | Stop handler is M3's fallback → M4 cond. |
| `POST /v1/faults` | M2 | flat `FaultRequest` |
| `POST /v1/traffic` | M3 | |
| `POST /v1/tenants/{id}/key` | M2 | |
| `GET /v1/tenants/{id}` | M1 (cut line, **expected cut**) → M4 cond. (+10 cmd, +20 world) | closed: M4 budgets `world.Tenant`/`TenantDetail` on M1's expected-cut path (M4 CR-5) |
| `POST /v1/reset` | M5 | |
| `GET /health` | M0 → M1 (`HealthInfo` + insert instrument) → M5 (`Current` release) | |

## 3. Parameters

### 3a. SPEC › The CMEK core parameter table

| Parameter (demo) | Milestone (`Params` field) |
| --- | --- |
| Lease hard TTL 30 s | M2 (`Lease`) |
| Soft TTL 15 s | M2 (`SoftTTL`) |
| KMS call timeout 500 ms | M1 (`KMSTimeout`) |
| Transient backoff 0.5 s → 8 s ± 50 %, one probe | M2 (`BackoffMin/Max/Jitter`) |
| Revoked re-probe 5 s | M2 (`RevokedReprobe`) |
| DEK reuse limit 10,000 msgs / 10 min | M2 (`DEKMaxMessages`, `DEKMaxAge`) |
| Retention 10 min | M5 (`Retention`, `ExpireInterval`, `ExpireLimit`, unconditional; M5 CR-4) |

### 3b. SPEC › load settings table

| Setting | Milestone |
| --- | --- |
| Delivery capacity 8 workers × 12.5 ms ≈ 640/s | M1 (`Workers`, `SinkLatencyMin/Max`, `capacity_ps`) |
| Tenant rate limit 100/s, burst 200 | M3 (`TenantRate`, `TenantBurst`) |
| Tenant backlog cap 500 | M3 (`TenantBacklogCap`) |
| Global backlog cap 20,000 | M3 (`GlobalBacklogCap`, `FairShare`) |

### 3c. SPEC › Fakes › KMS faults fields

| Field | Milestone |
| --- | --- |
| mode (ok, fast-fail) per provider / per tenant | M2 (`kms.Mode`, `Scope`) |
| lognormal latency (p50, p99) | M2 |
| error rate | M2 |
| key state (enabled, disabled) | M2 (`Revoke`/`Restore`) |

### 3d. DESIGN-REFERENCE › Params, by field (which plan adds it to `params.go`)

| Block | Fields → milestone |
| --- | --- |
| Population | `Tenants` M0; `Providers`, `Seed` M1 |
| Key lease | `KMSTimeout`, `SweepInterval` M1; `Lease`, `SoftTTL`, `EarlyExpiry`, `BackoffMin`, `BackoffMax`, `BackoffJitter`, `RevokedReprobe`, `DEKMaxMessages`, `DEKMaxAge`, `ProviderInflight`, `TenantInflight`, `IngestWaiters` M2 |
| Queue | `Workers`, `ClaimBatch`, `ClaimTimeout`, `ReclaimInterval`, `IdlePoll`, `DBDir` (amends `DBPath`), `SyncMode`, `WALAutocheckpoint` M1; `TwoClassSched`, `SchedLightTurns` (32, amends 4) M3; `Retention`, `ExpireInterval`, `ExpireLimit` M5 |
| Sink | `SinkLatencyMin/Max` M1; `SinkRing` M5 |
| Admission | `RetryAfter` M1; `TenantRate`, `TenantBurst`, `TenantBacklogCap`, `GlobalBacklogCap`, `FairShare` M3 |
| Traffic | `BaseRate`, `ZipfExponent`, `PayloadBytes`, `CanaryPrefix` M1 |
| Metrics/SSE | `SnapshotInterval` M0; `AuditRing`, `ViewerQueue` M1; `TimelineRing`, `SnapshotEvents` M2; `ChartWindow`, `P99Window`, `BaselineTicks`, `L1Ratio`, `L1Floor`, `L1Grace`, `L4CapacityFactor`, `L4Settle`, `TimelineAggregateMin` M4 (duplicates removed, M4 CR-3); `StreamMaxAge`, `StreamWriteTimeout` M5 |
| Checker | `CheckInterval`, `CanaryFullScan` M5; `Lights` (new) M5 |
| Lifecycle | `IdleRebuild` M0; `StopTimeout` M1 (pulled forward) |
| Scenarios | `SurgeTenantRank`, `TenantSurgeMult`, `TenantSurgeFor`, `GlobalSurgeMult`, `GlobalSurgeFor`, `GlobalSurgeAffectedTop` M3; `OutageBlip`, `OutageLong`, `OutageProvider`, `SlowProvider`, `SlowP50`, `SlowP99`, `SlowFor`, `RevokeTenantRank` M4; `ScenarioTailMin`, `ScenarioTailMax` M3 (closed: M3 CR-1; M4's `tail` reads them) |

## 4. Scenarios (SPEC › Fakes and scenarios)

| Scenario / button | Milestone | Card copy | Acceptance observed |
| --- | --- | --- | --- |
| Provider outage: 10 s blip button | M4 (`provider_blip`) | M4 step 1 (Ben) | M4 step 5 (Ben, browser) |
| Provider outage: 60 s outage button | M4 (`provider_outage`) | M4 | M4 step 4 (session, curl) |
| Key revocation (+ Restore button, ground-truth line) | M4; checker line M5 | M4 | M4 steps 5, 8 (live) |
| Slow KMS (azure p50 400 ms / p99 3 s) | M4 (`slow_kms`) | M4 (no "pins at 32") | M4 steps 4, 7; the snapshot `inflight{}` is built in M4 (M4 CR-1) |
| Tenant surge (rank 5 ×100, 60 s) | M3 (`tenant_surge`) | M4 | M3 step 9, M4 step 6 |
| Global surge (×5, 90 s) | M3 (`global_surge`) | M4 | M3 ×10 proxy; full run only in M5's script |
| Scenario tail / recovery_s / drain_s / restored line | M4 (`tail`, `SetCleared`, `Recovered`) | — | reads M3's `ScenarioTailMin/Max`; drained = affected backlog ≤ `Workers × ClaimBatch` (M4 CR-7) |
| Declared target set → affected series, ring | M3 (`targets` closures) → M4 (`SetTargets`) | — | |

## 5. UI regions (SPEC › Web UI)

| Region | Milestone | Note |
| --- | --- | --- |
| Header: three sentences, shared-system notice, Reset | M4 | Reset live in M5 |
| Scenario cards: title, what to watch, Start/Stop, countdown | M4 (`renderCards`); countdown data M3 (`SetScenario`) | Restore button M4 |
| Tenant grid 40 × 25 canvas, provider bands, four colours, ring | M4 (`paintGrid`) | grid string M1, `& 4` M4 |
| Grid hover (tenant, provider, lease remaining, backlog) | M4 upside (cut line) | needs `GET /v1/tenants/{id}` |
| Grid click → pinned detail, last 10 audit entries | M4 upside (cut line) | `TenantAudit` M4 |
| Tiles: delivered vs capacity (+ `#l4-split`), healthy p99, KMS calls/s (+ `#inflight`), tenants by state | M4 (`renderTiles`) | `#inflight` data built in M4 (CR-1) |
| Chart 1 ingest outcomes per second | M4 (base) | |
| Chart 2 e2e p99 healthy vs affected | M4 (base, first) | |
| Chart 3 KMS calls/s by class vs events/s | M4 upside (cut line "drop to 2 charts") | |
| Chart 4 backlog total + affected | M4 upside | |
| Invariant panel S1–S4, L1, L4: light, counter, last-checked | M4 (`renderPanel`) + M5 (`Report`, `invariants`) | |
| Event timeline, newest first, aggregation | M2 (lines) → M4 (aggregation, `renderTimeline`) | |
| Starting state before first snapshot; world-id change clears | M4 (`connect`, `onSnapshot`) | |
| Reconnect on `event: reconnect` / Cloud Run cut | M4 (`connect`) + M5 (`StreamMaxAge`, C3 optional) | |

## 6. Invariants and SLO lights (SPEC › Correctness contract)

| ID | Live check | Milestone | Note |
| --- | --- | --- | --- |
| S1 No plaintext at rest | canary full scan every 5 s | M5 (`CanaryFull`, S1) | M1 manual `grep` on the tmpfs file |
| S2 Tenant key isolation | AAD (M1) + sink mismatch counter | M1 (`Mismatches`) → M5 (S2 light) | |
| S3 Bounded revocation | ground truth + `DeliveredBetween`, detection latency | M5 | `sinceSeq` init amendment |
| S4 No loss from key unavailability | conservation per tenant via `Census` | M5 | sink bound kept |
| L1 Blast radius (light + healthy-vs-affected chart) | `HealthyP99`, `HealthyRejections` | M5 — **expected cut C1 → buffer 4:25–4:45** | chart M4 |
| L2 KMS call economy (chart 3 + tenant detail; no light) | — | M4 upside (chart 3) + tenant detail (M4 cond.) | no light per spec |
| L3 Self-healing (timeline + recovery time; no light) | — | M4 (`recovery_s`, restored line) | |
| L4 Overload (light + delivered tile + backlog chart + within/over split) | `DeliveredPS`, `OverloadedWithinShare` | M5 — **expected cut C1 → buffer**; tile/split M3–M4 | |

## 7. Checks (SPEC › Scope › Checks; P0_EXPLAINED › Checks)

| Check | Milestone |
| --- | --- |
| Live invariant checker goroutine, 1 Hz, outside trust boundary | M5 (`check.Checker`) |
| Plaintext canary in every payload / scan | M1 (payload) / M5 (S1) |
| Revocation check (ground truth t + lease, detection latency) | M5 (S3) |
| Conservation check | M5 (S4) |
| Unit tests: classifier table | M2 |
| Unit tests: lease with fake clock (renew, soft, hard, purge on deny) | M2 (`TestLease` + `TestManagerWalk`) |
| Unit tests: envelope (round trip, AAD mismatch) | M1 (M2 duplicates) |
| Manager walk extras (SC-F2, SC-F3, SC-F5, SC-F8 stale-OK, rotation, errBusy) | M2 → M5 buffer item, Ben, ≈ 40 lines (M2 CR-5, M5 CR-5) |
| Checker can turn red (`TestCheckerTurnsRed`) | M5 |
| Two Worlds in one process (`TestTwoWorlds`) | M1 (+2 lines M3) |
| Race storm test (SC-F4) | M5 buffer (C4) |
| Holder idle rule test | M5 buffer |
| `BenchmarkInsert`, `TestLedger` | M1 (Ben) |
| `TestHistP99` | M4 (Ben) |
| CI: `go vet`, no-globals grep, cmek import check | M0 (grep) / M2 (import check) / M4 (static line) |

## 8. World lifecycle (ARCH › World; SPEC › Web UI, D9)

| Item | Milestone |
| --- | --- |
| `World` struct, `Params`, `Demo()` | M0 (stub) → M1 |
| `Deps{ID, Clock, Logger}`, `New`, `Start`, `Stop` (≤ `StopTimeout`) | M1 |
| `Small()` | M1 |
| `Holder` `Ensure`/`Current` | M0 → M1 (`NewHolder(p, d)`, stated in M1 CR-1) |
| `Holder.Acquire` idle rule (> 10 s → rebuild) | M5 |
| `Holder.Reset` | M5 |
| Handler refcount, `Stop` closes subscribers first | M5 |
| Viewer count (hub), 1→0 pause, 0→1 resume, `idleSince` | M1 |
| Cmek sweep (`Tick` every 250 ms), queue sweeps | M1 (sweep loop) → M5 (`Reclaim`/`Expire` wired) |
| Goroutine inventory: traffic loops, workers, checker, metrics tick, scenario runner | M1 / M1 / M5 / M1 / M3 |
| M0 ticker check on Cloud Run + D9 fallback rule | M0 (`scripts/m0-ticker-check.sh`, thresholds) |
| SSE write deadline, `r.Context()` select, `StreamMaxAge` reconnect | M0 (select) → M1 (rewrite; select not restated) → M5 (C3) |
| `/health` ticks, uptime, world id, insert instrument | M0 → M1 |
| DB file per World, removed on Stop, stale-file pre-remove | M1 |

## 9. Deliverables (SPEC › Constraints and success criteria; HANDOFF)

| Deliverable | Milestone |
| --- | --- |
| Deployed URL on Cloud Run (min 0 / max 1, request billing, 3600 s timeout, 1 GiB) | M0 (first) → M4 (3:05 image) → M5 (final, 3:34) |
| Dockerfile, `.dockerignore`, go.mod pins (go 1.26) | M0 |
| Public GitHub repo `github.com/prabenzo/cmek`, `docs/SPEC.md` | M0 (exists) |
| Self-contained evaluation (fakes in-process, cards say what to watch) | M1–M4 |
| Scoping: cut line per milestone, stretch after P0 | every plan |
| ~5 min video | M6 |
| Short written doc `docs/RATIONALE.md` | M6 |
| AI transcripts `docs/transcripts/` + index | M6 |
| Time spent `docs/TIMELOG.md` (created M0, row per box, Total M6) | M0 → M6 |
| README (thesis, URL, run, scenario guide, deploy recipe, links) | M5 (30 lines) + M6 (+40: scenario guide, stale-OK sentence, rationale and transcript links, time spent; M6 CR-1) |
| Decision log reflecting who decided (D1–D9 + build-time calls) | M6 |
| `internal/cmek` near 500 lines, Ben-owned | M2 (`wc -l` ≤ 560) |
| Optional `.github/workflows/deploy.yml` | not planned (ARCH: optional, Ben after M0) |

## 10. Stretch (SPEC › Scope)

| Item | Status |
| --- | --- |
| X1 Naive-mode switch for slow KMS | stretch, not planned (M2/M4 mention as an alternative only) |
| X2 Per-session worlds | stretch, not planned (M1's `DBDir`/`Deps.ID` keep it cheap) |
| X3 Brownout surge, KMS 429 class, blackhole, cold start, key rotation, lease slider | stretch, not planned |
| X4 Scenario tests in virtual time (`testing/synctest`) | stretch, not planned (M5 cites for positive S3) |
| "With more time" table | rationale only (M6 § 6) |

## 11. Decisions for Ben — every bullet across M0–M6, deduplicated

| Decision | Plan(s) | One-line summary (recommended / alternative) | ARCH bullet? |
| --- | --- | --- | --- |
| Deploy path | M0 | docker build + push + `gcloud run deploy --image` / `--source` (recompiles sqlite each deploy) | yes |
| Cross-compiling builder stage | M0 | `--platform=$BUILDPLATFORM` + `GOARCH=$TARGETARCH` / `gcloud builds submit` (0:10 abort path) | plan-added |
| Tick burn `KS_TICK_BURN_MS=50` | M0 | 50 ms burn per tick so A/B discriminate / 0 (bare increment) | plan-added |
| Nonce source | M1 | `Seal` reads `crypto/rand` directly, `Config.Nonce` deleted / `Handle` carries a reader | plan-added (amends ARCH) |
| `DBDir` + `Deps.ID` | M1 | directory param + id from Holder / keep `DBPath`, `New(id, p, d)` | plan-added (amends ARCH) |
| Measurement trigger | M1 | `insert_mean_us ≤ 300` sole trigger / keep `insert_max_us ≤ 2000` | plan-added (amends ARCH) |
| Insert strategy and fallbacks | M1 | per-event insert, NORMAL, ladder (1)+(2) then group commit / group commit day one | yes |
| M1 tests beyond `BenchmarkInsert` | M1 | `TestLedger` + `TestEnvelope` on Ben's lane / ARCH slack-only rule | plan-added (variant of ARCH "Tests") |
| Who writes `envelope.go` / `world_test.go` in M1 | M1 | Ben in parallel / Claude drafts | plan-added |
| Load generator shape | M1 | 1,000 goroutines / timer heap + pool | yes |
| Id allocation and S1 scan | M1 | `NextID()` + plain Insert + full scan / seal callback + incremental | yes |
| Audit storage | M1 | `Auditor` + 15-line bridge / metrics imports cmek | yes |
| Retry-After values | M1 | fixed Params by sentinel / `RetryAfterer` | yes |
| Provider mapping | M1 | contiguous bands + shuffled ranks / `t % 3` | yes |
| Grid encoding | M1 | 1,000-char string / base64 + target list | yes |
| 202 body id | M1 | `IngestID` / `Ingest` only | plan-added |
| Cold-fetch failure edge + blip card | M2 (card wording M4) | compose two edges in one `apply` / explicit README edge | yes |
| M2 split | M2 | two lanes / Ben writes all of cmek | yes |
| Slow-azure rows + in-flight tile: M2 or M4 | M2 | M4 / keep in M2 with reduced assertion | plan-added |
| Walk extras: M5 or M2 | M2 | M5 buffer item (Ben) / Ben writes in M2 | plan-added (carried by M5 CR-5) |
| Provider bulkhead full | M2 | wait within 500 ms deadline / fail fast | yes |
| Tenant cap or waiter cap full at ingest | M2 | fast 503 key_unavailable / 503 overloaded | yes |
| Stale-OK guard | M2 | keep 2-line guard / delete | yes |
| DEK rotation during ride-through | M2 | keep sealing under exhausted DEK / strict bound | yes |
| Manager locking | M2 | one `m.mu` + atomic states / per-tenant mutexes | yes |
| Scheduler class | M3 | two-class interleaved ring, `SchedLightTurns = 32` (amends 4) / literal ring | yes |
| Global-surge cap visibility | M3 | `GlobalSurgeFor = 90 s` / `GlobalBacklogCap = 12,000` | yes |
| M3 slack use | M3 | Ben's lane: `Expire`, then `Census` / Ben on M4 card copy | yes |
| Who drafts the two-class `Next` | M3 | Ben in parallel / Claude | plan-added |
| L1 floor | M4 | 25 ms / 0 | yes |
| L4 tolerance | M4 | 0.9 × measured plateau / 1.0 × measured | yes |
| Tenant-surge KMS-call wording | M4 | card says "plus one rotation per 10,000" / `DEKMaxMessages = 100,000` | yes |
| Hover/pin and the two ordering deviations | M4 | aggregation early, hover/pin at 3:03 trigger / ARCH order verbatim | yes |
| p99 quantization | M4 | linear interpolation in bucket / ratio 1.2 with 61 buckets | plan-added (M5's duplicate deleted, M5 CR-2) |
| Revocation restore | M4 | manual Restore / auto-restore 45 s | yes |
| Revoke target rank | M4 | rank 3 / rank 1 | yes (half of ARCH "Scenario targets") |
| Scenario targets: surge rank and affected set | M3 | `SurgeTenantRank = 5`, `GlobalSurgeAffectedTop = 150` / rank 1, top 50 | yes (other half; M3 CR-2) |
| Revocation timeline lines | M5 | two lines per episode (service audit + checker ground truth) / checker-only single line | yes (M5 CR-6) |
| `#inflight` under slow KMS | M4 | card does not claim 32; must-see azure ≥ 5 and ≥ 5× / `SoftTTL` or `KMSTimeout` lever | plan-added |
| C1 by evidence at 3:15 | M5 | decide L1/L4 from TIMELOG; expected cut / attempt in box | plan-added |
| Stream lifetime | M5 | `StreamMaxAge = 55 min` + `event: reconnect` / rely on Cloud Run cut | yes |
| Holder phasing | M5 (M0 defers) | grow in place, `TestHolderIdleRule` as buffer / prove by curl only | yes |
| Tests | M5 (M1 variant) | `TestCheckerTurnsRed` required; storm + idle tests buffer / add store_test rows | yes |
| Ben's parallel stream in M5 | M5 | Ben drafts `store.go` + `holder.go` + reset / ARCH fallback (README from 3:25) | plan-added |
| Scenarios in the video | M6 | revocation → outage → tenant surge / global surge instead | plan-added |
| Video format | M6 | one continuous take in a pause-capable recorder / silent capture + voice-over | plan-added |
| Transcript form | M6 | raw files + index / curated excerpts | plan-added |
| Time-spent reporting | M6 | build clock + planning line / build clock only | plan-added |
| RATIONALE self-containment | M6 | copy spec tables / link + 40 lines prose | plan-added |

### ARCH › Decisions for Ben not carried by any milestone plan

None after the fix pass. The two the critic found are now carried: **Revocation timeline lines** by M5 (CR-6, with M4's revocation row as context) and the surge-rank/top-150 half of **Scenario targets** by M3 (CR-2). All 28 ARCH decisions are carried by the milestone ARCH-NOTES 5 names (cold-fetch → M2; scheduler class, global-surge visibility, M3 slack, scenario targets → M3; L1 floor, L4 tolerance, surge card wording, hover/pin, revoke rank, revocation restore → M4; stream lifetime, holder phasing, tests, revocation timeline lines → M5; deploy path, insert strategy, load generator, id allocation, audit storage, Retry-After, provider mapping, grid encoding → M0/M1).
