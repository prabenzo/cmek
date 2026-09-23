# No key cache — a sixth scenario that shows what the lease and the DEK cache are for (before M5)

> References: `DESIGN-REFERENCE.md` is the detailed design; "ARCH › section" is shorthand for it. This plan amends it where marked **amends ARCH**. Status at the bottom.

## Overview

**What this change accomplishes.**

- A sixth card, **No key cache**, runs one provider's tenants (gcp, 333 cells) the way a naive CMEK integration works: no lease, no cached data key. Every event costs one KMS round trip to seal at ingest and another to open at delivery, the way Tink's `KMSEnvelopeAEAD` would (the design the Tink plan rejected on purpose). The other two providers keep the cache, and all three providers run at a realistic KMS latency (p50 20 ms, p99 80 ms) for the whole run, so the only variable between the gcp third and the rest is the cache.
- The run has three phases, 55 s in all. **Cache off (30 s)**: KMS calls/s jump from ≈ 40 to ≈ 240 (two per gcp event; chart 3 and the tile), the affected p99 line on chart 2 sits ≈ 40–100 ms above the healthy line (two KMS round trips on the request path), in-flight gcp calls rise from 0 to ≈ 5–10, and the gcp tenants' detail shows 0 hot DEKs. **The same gcp blip (10 s)** that the Provider outage card shows as harmless: with no lease to ride on, every gcp cell turns orange within a second (one aggregated timeline line, `333 gcp tenants ACTIVE → KEY_UNAVAILABLE`), ingest answers `503 key_unavailable` for a third of the tenants at once and their delivery parks. **Cache off (15 s more)** after the blip clears: the tenants come back on the normal probe schedule; then the exit switches the cache back on, clears the latency, and the usual tail measures recovery and drain. A summary line posts at the end with the numbers side by side (KMS calls/s gcp vs aws, affected vs healthy p99, tenants parked by the blip and how fast).
- The lesson the card states: the cache and the lease buy three things at once, and the scenario removes all three. KMS calls stop being flat (L2 goes red for the run, and the card says so). The KMS round trip lands on the request path (latency). A KMS blip becomes an outage (no ride-through). Nothing else changes: bulkheads, backoff, the state machine, the sink, admission, and the four safety invariants all hold (S4 in particular: parked, never dropped).
- What does **not** change: the cached design for every other tenant and every other card; the queue, scheduler, workers; the metrics pipeline (one counter added); the checker's plan (one note added for M5).

**What is deliberately not done.**

- No global switch. Turning the cache off for all 1,000 tenants would empty the healthy p99 series (everyone affected) and lose the side-by-side comparison that makes the point; one band next to two cached bands is the picture. A global variant is listed under Decisions.
- No new failure semantics. Pass-through mode does not add per-request retries or remove backoff: a gcp tenant whose call fails is parked and probed on the same 0.5 → 8 s schedule as today, so the run isolates the cache, not the retry policy. (A naive design with no backoff either would hot-loop the KMS during the blip; that is a different lesson and would bleed into the healthy tenants through the worker pool.)
- No HTTP endpoint for the switch (Decisions).

**Size and where it lands.** ≈ 200 lines of Go across `cmek` (the pass-through paths), `world` (the scenario, two setters, six parameters), `metrics` (per-provider call counts for the summary line), plus ≈ 15 lines of card copy and ≈ 90 lines of tests. On `claude/epic-rubin-kgewov` after the M4 merge, before M5, as its own pull request. ≈ 35 working minutes.

## Files

| Path | Change | Purpose | ~Lines |
| --- | --- | --- | --- |
| `internal/cmek/lease.go` | Mod | `tenant.passThrough bool` (under `t.mu`): the cache is never read and never filled for this tenant | +2 |
| `internal/cmek/manager.go` | Mod | `SetPassThrough(id string, on bool)`: sets the flag and, on entering, purges the plaintext primitives (audit `purge`/`cache off`); `EncryptKey` and `DecryptKey` take the pass-through branch (Interfaces); `handle()` returns false while pass-through; `Hot()` is `state == ACTIVE` with no kick; `Warm()` is a no-op (the worker's `DecryptKey` is the fetch); `Tick` keeps the probe schedule and skips the lease-lapse purge for the tenant | +45 |
| `internal/cmek/fetcher.go` | Mod | `apply`: while pass-through, a lease is never "usable" (a Transient goes straight to KEY_UNAVAILABLE with detail `no cache: call failed`, never RIDING_THROUGH) and an OK does not store `d.prim` (0 hot DEKs); everything else, including the Deny row and the stale-OK guard, unchanged | +8 |
| `internal/world/params.go` | Mod | `NoCacheProvider "gcp"`, `NoCacheFor 30s`, `NoCacheBlip 10s`, `NoCacheAfter 15s`, `NoCacheKMSP50 20ms`, `NoCacheKMSP99 80ms` (`Demo()` and `Small()` values) | +8 |
| `internal/world/world.go` | Mod | `SetCache(provider string, on bool)`: `SetPassThrough` for every tenant of the band; a helper to set the run's latency on all providers and clear it | +20 |
| `internal/world/scenarios.go` | Mod | `s.defs["no_cache"]`: scope `NoCacheProvider`, targets its band, marker, phases `cache_off` (enter: latency on all providers, cache off), `blip` (enter: fast-fail on the provider, latency kept), `cache_off` (enter: fast-fail cleared, latency kept, post the blip's parked count), exit: cache on, all faults cleared, summary line | +40 |
| `internal/metrics/registry.go` | Mod | per-provider KMS call counter per tick (`Audit` already knows the provider from `Idx`), exposed as `ProviderCallsPS(p) float64` for the summary line; not in the snapshot (chart 3 stays by class) | +15 |
| `web/index.html` | Mod | the sixth card (copy below), `data-scenario="no_cache"`, Start and Stop | +6 |
| `web/app.js` | Mod | nothing: cards are generic (`data-start`, `data-stop`, the status line) | 0 |
| `internal/cmek/cmek_test.go` | Mod | `TestPassThrough` (Tests) | +60 |
| `internal/world/world_test.go` | Mod | a `no_cache` row in `TestScenarioRunner` (three phases, the exit restores the cache and clears the faults) | +20 |
| `internal/metrics/metrics_test.go` | Mod | a `ProviderCallsPS` row | +10 |
| `docs/SPEC.md`, `docs/plan/DESIGN-REFERENCE.md`, `M5.md`, `M6.md`, `COVERAGE.md` | Mod | SPEC › Scenarios gains the card; the amendments row; M5's every-scenario script runs seven starts (the sixth card) and expects the L2 light red during it; M6's scenario guide has six cards | ≈ 10 lines |

## Interfaces and types (after)

```go
package cmek

// SetPassThrough switches one tenant between the cached design (off) and per-request KMS calls (on). Entering
// pass-through drops every cached primitive (audit purge "cache off"); leaving it lets the next probe or
// renewal warm the cache again. Everything else (state machine, backoff, bulkheads, the Deny row) is unchanged.
func (m *Manager) SetPassThrough(id string, on bool)
```

The two hot paths while `t.passThrough` (unexported; the only new code paths):

```go
// EncryptKey, pass-through branch (after the REVOKED / KEY_UNAVAILABLE early returns, which stay: a parked
// tenant is probed on the backoff schedule, not on every request):
//   d := t.active (under t.mu)
//   if d == nil { d, r = m.generate(t) }                       // first event: one generate, as today
//   else        { r = m.call(t, "unwrap", d); m.apply(t, "unwrap", d, r) }   // one KMS call, no singleflight
//   OK        → Handle{DEKID: d.id, ValidUntil: t.lease.Until(), prim: r.prim}   // the primitive is not cached
//   Transient → ErrKeyUnavailable (apply has already parked the tenant: KEY_UNAVAILABLE, no ride-through)
//   Deny      → ErrKeyRevoked
//   errBusy   → ErrKeyUnavailable (the tenant cap: two calls in flight per tenant; see Decisions)

// DecryptKey, pass-through branch (after the ErrPoison / REVOKED / KEY_UNAVAILABLE returns):
//   r := m.call(t, "unwrap", d); m.apply(t, "unwrap", d, r)
//   OK → Handle{...prim: r.prim}; Transient → ErrKeyUnavailable (the worker releases the batch: parked, not dead)
```

```go
package world

// SetCache turns the key cache on or off for every tenant of one provider (the no_cache scenario's switch).
func (w *World) SetCache(provider string, on bool) error
```

```go
package metrics

// ProviderCallsPS is the last tick's KMS calls per second for one provider (all classes), for the summary line.
func (r *Registry) ProviderCallsPS(provider string) float64
```

Scenario definition:

```go
s.defs["no_cache"] = &scenario{
    name: "no_cache", scope: p.NoCacheProvider,
    marker:  "scenario no_cache started: gcp tenants run without the key cache for 55 s; every event is two KMS calls; all providers at p50 20 ms",
    targets: func() []int { return w.band(p.NoCacheProvider) },
    phases: []phase{
        {name: "cache_off", dur: p.NoCacheFor,   enter: func() { latencyAll(on); w.SetCache(prov, false) }},
        {name: "blip",      dur: p.NoCacheBlip,  enter: func() { fault(prov, fast_fail + latency) }},
        {name: "cache_off", dur: p.NoCacheAfter, enter: func() { fault(prov, ok + latency); postParked() }},
    },
    exit: func() { w.SetCache(prov, true); latencyAll(off); postSummary() },
}
```

Card copy (`web/index.html`, sixth card, Claude drafts, Ben reviews with the page):

> **No key cache.** gcp tenants run without the key cache for 55 s, the way a naive integration works: every event is one KMS call to seal and another to deliver. All three providers run at a realistic 20 ms for the run, so only the cache differs. KMS calls/s jump from ~40 to ~240 and the L2 light goes red: that is the point. The affected p99 line climbs 40–100 ms above the healthy one; calls in flight to gcp rise. At 30 s the same 10 s gcp blip the outage card shows as harmless turns all 333 gcp cells orange within a second: `503 key_unavailable`, delivery parked, nothing lost. The cache returns at 55 s.

Summary line (posted by exit, before the tail): `no cache (gcp, 55 s): KMS calls gcp 238/s vs aws 13/s · p99 affected 96 ms vs healthy 7 ms · blip: 333 tenants KEY_UNAVAILABLE in 0.5 s`. Values read from `ProviderCallsPS`, the snapshot's p99 pair at the end of the first phase, and `States` one second into the blip.

**amends ARCH** › Scenarios: six cards; › cmek › `DecryptKey` "never calls a KMS" gains "unless the tenant is in pass-through"; › Data flow (b) delivery: the worker path is unchanged (a pass-through `DecryptKey` error is released like a lapsed lease).

## Order of work (one lane, Claude; Ben reviews the pull request)

1. **T+0–12 cmek.** `passThrough`, `SetPassThrough`, the two branches, `apply`'s two conditions, `Hot`/`Warm`/`Tick` guards; `TestPassThrough`; `go test -race ./internal/cmek` (the walk, the extras and the concurrent cold callers must pass unchanged: no cached tenant sees a different path).
2. **T+12–22 world and metrics.** Parameters, `SetCache`, the latency helper, the scenario with its three phases and the two posted lines, `ProviderCallsPS`; the runner test row; `go test -race ./...`.
3. **T+22–26 web.** The card; headless check (card renders, Start disables the others, status shows the phase and countdown).
4. **T+26–34 acceptance.** Local run at 300/s on `/dev/shm` from the stream: baseline, then `no_cache`: `kms_ps.ok` ≈ 6× baseline and `inflight.gcp` ≥ 5 within 5 s; `p99_ms.affected − p99_ms.healthy` ≥ 40 ms by 20 s with `p99_ms.healthy` within 1.25× its baseline (L1 for the cached two thirds); at blip + 1 s `by_state[2]` ≥ 300 with one aggregated timeline line and `ingest_ps.key_unavailable` > 0; after the blip clears, `by_state[2]` → 0 within 12 s; at exit the summary line; tail `gcp restored: 333 tenants ACTIVE in x s, backlog drained in y s`; `ingest_ps.internal` 0; 0 unexpected service-log errors; a second run of `provider_blip` afterwards behaves as in M4 (the cache is back).
5. **T+34–35 docs.** SPEC scenarios row, amendments row, M5/M6 notes, README decisions row, TIMELOG row.

## Tests

| Test | File | Asserts |
| --- | --- | --- |
| `TestPassThrough` (new) | cmek_test.go | rig: warm the tenant (one generate); `SetPassThrough(on)` audits a purge and `Info().HotDEKs == 0`; three `EncryptKey` calls make three KMS calls (no singleflight, nothing cached) and each handle seals; `DecryptKey` makes one call per request and opens; a `SetFault(fast-fail)` then `EncryptKey` → `ErrKeyUnavailable` at once and state `KEY_UNAVAILABLE` (never `RIDING_THROUGH`), one `unwrap error` audit with detail `no cache: call failed`; `Tick` after the backoff probes on schedule and the tenant returns to ACTIVE; `Revoke` then `EncryptKey` → `ErrKeyRevoked`, REVOKED, one `REVOKED, 0 DEKs purged` line; `SetPassThrough(off)` → the next `EncryptKey` after a probe serves from the cache with no call |
| `TestScenarioRunner` (row) | world_test.go | `no_cache` with `Small()` and short phases: `SetCache(false)` applied to the band only (a tenant outside it keeps `HotDEKs`), the three phases in order, exit restores the cache and clears every provider's fault, the summary line is posted, the tail runs |
| `ProviderCallsPS` (row) | metrics_test.go | three audits for one provider's tenants and one for another in one tick → 3/0.5 s and 1/0.5 s |
| existing walk, extras, concurrency, scenario runner, hist, transitions, gate | unchanged | green under `-race` |

## Decisions for Ben

**Status: plan drafted 2026-09-23; awaiting Ben's approval before any code (standing process). Each bullet's first option is the recommendation.**

- **Scope: one provider's band without the cache (recommended) vs every tenant.** One band next to two cached bands gives the side-by-side: same World, same KMS latency, the affected p99 line against the healthy one, one third of the grid orange in the blip while two thirds ride it out. A global switch would show the same call multiplication but no comparison, and it empties the healthy p99 series (everyone is a target) so L1 has nothing to compare.
- **Realistic KMS latency on all three providers for the run (recommended) vs only on the pass-through provider.** With the fake's default zero latency, per-request KMS calls cost microseconds and the p99 line would not move; 20 ms is a fair cloud-KMS figure. Applying it to all three providers makes the comparison honest: the cached tenants pay the same latency and do not feel it. Levers: `NoCacheKMSP50/P99`.
- **The tenant in-flight cap stays at 2 during pass-through (recommended) vs raising it.** The top gcp tenants send ≈ 5–15 events/s; at 20 ms per call that is well under two in flight, so `errBusy` 503s should be rare; if the acceptance run shows them, they are a real cost of per-request calls under a cap sized for the cached design and the card can say so. Alternative: lift the cap for pass-through tenants (one line) to keep the message purely about latency and the blip.
- **The L2 light goes red during the run and the card says so (recommended) vs teaching the checker to exempt the targets.** Red is the lesson; M5's every-scenario script expects red L2 for this run and green everywhere else. Alternative: M5's checker excludes pass-through tenants from L2 (a few lines, but then the panel hides the point).
- **Scenario-only switch (recommended) vs a `POST /v1/cache` endpoint.** The card is the demo; an endpoint would be another thing to document and to reset. Alternative: the endpoint, so the video can toggle it by hand.
- **Recovery inside the run keeps the backoff schedule (recommended) vs per-request retries.** Isolates the cache as the one variable (see "deliberately not done"). Alternative: a `naive` flag that also retries per request, showing the hot loop; more dramatic, harder to read, and it spills into the healthy tenants through the worker pool.
- **Phase lengths 30 + 10 + 15 s (recommended).** Long enough for the p99 window (10 ticks = 5 s) to settle before the blip and for the tenants to recover on the probe schedule (≤ 12 s) before the cache returns, short enough for the video. Levers: `NoCacheFor`, `NoCacheBlip`, `NoCacheAfter`.

## Risks

- **Healthy p99 during the run.** The gcp deliveries each hold a worker for an extra ≈ 20 ms; at 300/s that is ≈ 2 worker-seconds per second on an 8-worker pool running at ≈ 47 % (M4's plateau), so the cached tenants' p99 should stay flat, but the pool is a shared resource and a rise would be a real blast-radius finding of the naive design, not a bug. The T+26 run reads `p99_ms.healthy` against its baseline; the lever is `NoCacheKMSP50` (10 ms) before anything structural.
- **Timeline volume.** The blip turns ≈ 333 tenants orange in one flush (one aggregated line) and green again over ≈ 12 s of probes (a few aggregated lines): fewer lines than the outage card, not more.
- **Audit rings.** A pass-through tenant writes one `unwrap` audit per event; the 10-entry ring and the per-minute call count in the tenant detail then show exactly that (≈ 300–900 calls/min for a top gcp tenant): the number the card wants a viewer to see.
- **M5 coupling.** The every-scenario script gains one run (≈ 55 s + tail) and one expectation (L2 red); the checker's S4 conservation holds through it (parked rows are counted as queued). M5's plan is amended in its dependencies, nothing in its design.

## Status

**Awaiting approval.** No code beyond this document. On approval the build runs on `claude/epic-rubin-kgewov` before M5, Ben opens its pull request into `main`, and M5's plan is amended for the seventh run and the L2 expectation.
