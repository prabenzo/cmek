# No key cache — a sixth card that shows what the lease and the DEK cache are for (before M5)

> References: `DESIGN-REFERENCE.md` is the detailed design; "ARCH › section" is shorthand for it. This plan amends it where marked **amends ARCH**. Status at the bottom.

## Overview

**What this change accomplishes.**

- A sixth card, **No key cache**, with two buttons, runs tenants the way a naive CMEK integration works: no lease, no cached data key. Every event costs one KMS round trip to seal at ingest and another to open at delivery, the way Tink's `KMSEnvelopeAEAD` would (the design the Tink plan rejected on purpose). In both runs all three providers run at a realistic KMS latency (p50 20 ms, p99 80 ms), so the only variable is the cache.
- **One band + blip (55 s)** switches the cache off for one provider's tenants (gcp, 333 cells) while the other two thirds keep it: the side-by-side. **Everyone + surge (90 s)** switches it off for all 1,000 tenants and then sends the global surge through it: what happens to a service designed without the cache when it gets popular.
- The one-band run has three phases, 55 s in all. **Cache off (30 s)**: KMS calls/s jump from ≈ 40 to ≈ 240 (two per gcp event; chart 3 and the tile), the affected p99 line on chart 2 sits ≈ 40–100 ms above the healthy line (two KMS round trips on the request path), in-flight gcp calls rise from 0 to ≈ 5–10, and the gcp tenants' detail shows 0 hot DEKs. **The same gcp blip (10 s)** that the Provider outage card shows as harmless: with no lease to ride on, every gcp cell turns orange within a second (one aggregated timeline line, `333 gcp tenants ACTIVE → KEY_UNAVAILABLE`), ingest answers `503 key_unavailable` for a third of the tenants at once and their delivery parks. **Cache off (15 s more)** after the blip clears: the tenants come back on the normal probe schedule; then the exit switches the cache back on, clears the latency, and the usual tail measures recovery and drain. A summary line posts at the end with the numbers side by side (KMS calls/s gcp vs aws, affected vs healthy p99, tenants parked by the blip and how fast).
- The everyone-plus-surge run has three phases, 90 s in all. **Cache off for everyone (15 s)**: before any surge, KMS calls/s go from ≈ 40 to ≈ 600 (two per event for 300 events/s) and the delivered tile drops from ≈ 300/s to ≈ 210–250/s: each delivery now holds a worker for an extra KMS round trip, so the eight workers deliver ≈ 8 / (12.5 ms + 25 ms) ≈ 215/s and the service falls behind its own baseline load; the backlog starts climbing at ≈ 50–90 rows/s and every band's p99 rises together. **The global surge (60 s, the same 5× the Global surge card sends)**: accepted ≈ 1,500/s until the backlog cap (≈ 15 s in), delivered stays pinned near ≈ 215/s (L4 red: capacity is 620), KMS calls ≈ 1,700/s, in-flight per provider climbs toward the bulkhead (32) and its tail waits push calls past the 500 ms timeout, so tenants across all three bands go orange from KMS timeouts and their events answer `503 key_unavailable` on top of the `503 overloaded` shedding; L1, L2 and L4 are red at once. **Cache off, 15 s more** after the surge ends, then the exit restores the cache and the latency and the tail measures recovery and drain (long: the backlog is at its cap and drains at ≈ 300/s). The contrast is the Global surge card: the same 1,500/s with the cache holds delivered at ≈ 620/s with ≈ 67 KMS calls/s and 960 tenants noticing nothing.
- The lesson the card states: the cache and the lease buy three things at once, and the runs remove all three. KMS calls stop being flat (L2 goes red for the run, and the card says so). The KMS round trip lands on the request path (latency, and with it delivery capacity). A KMS blip becomes an outage (no ride-through). Nothing else changes: bulkheads, backoff, the state machine, the sink, admission, and the four safety invariants all hold (S4 in particular: parked, never dropped).
- What does **not** change: the cached design for every other tenant and every other card; the queue, scheduler, workers; the metrics pipeline (one counter added); the checker's plan (one note added for M5).

**What is deliberately not done.**

- No global switch outside a scenario. The everyone-plus-surge run is the global variant; it is a card button with a fixed script, not a mode the World can be left in (Decisions: endpoint).
- No new failure semantics. Pass-through mode does not add per-request retries or remove backoff: a gcp tenant whose call fails is parked and probed on the same 0.5 → 8 s schedule as today, so the run isolates the cache, not the retry policy. (A naive design with no backoff either would hot-loop the KMS during the blip; that is a different lesson and would bleed into the healthy tenants through the worker pool.)
- No HTTP endpoint for the switch (Decisions).

**Size and where it lands.** ≈ 230 lines of Go across `cmek` (the pass-through paths), `world` (two scenarios, the setters, nine parameters), `metrics` (per-provider call counts for the summary lines), plus ≈ 20 lines of card copy and ≈ 100 lines of tests. On `claude/epic-rubin-kgewov` after the M4 merge, before M5, as its own pull request. ≈ 40 working minutes.

## Files

| Path | Change | Purpose | ~Lines |
| --- | --- | --- | --- |
| `internal/cmek/lease.go` | Mod | `tenant.passThrough bool` (under `t.mu`): the cache is never read and never filled for this tenant | +2 |
| `internal/cmek/manager.go` | Mod | `SetPassThrough(id string, on bool)`: sets the flag and, on entering, purges the plaintext primitives (audit `purge`/`cache off`); `EncryptKey` and `DecryptKey` take the pass-through branch (Interfaces); `handle()` returns false while pass-through; `Hot()` is `state == ACTIVE` with no kick; `Warm()` is a no-op (the worker's `DecryptKey` is the fetch); `Tick` keeps the probe schedule and skips the lease-lapse purge for the tenant | +45 |
| `internal/cmek/fetcher.go` | Mod | `apply`: while pass-through, a lease is never "usable" (a Transient goes straight to KEY_UNAVAILABLE with detail `no cache: call failed`, never RIDING_THROUGH) and an OK does not store `d.prim` (0 hot DEKs); everything else, including the Deny row and the stale-OK guard, unchanged | +8 |
| `internal/world/params.go` | Mod | `NoCacheProvider "gcp"`, `NoCacheFor 30s`, `NoCacheBlip 10s`, `NoCacheAfter 15s`, `NoCacheKMSP50 20ms`, `NoCacheKMSP99 80ms`; `NoCacheSurgeLead 15s`, `NoCacheSurgeFor 60s`, `NoCacheSurgeAfter 15s` (the surge multiplier and the declared affected set are `GlobalSurgeMult` and `GlobalSurgeAffectedTop`, so the two surge cards are comparable frame for frame) (`Demo()` and `Small()` values) | +11 |
| `internal/world/world.go` | Mod | `SetCache(provider string, on bool)`: `SetPassThrough` for every tenant of the band, or of every band when `provider == ""`; a helper to set the run's latency on all providers and clear it | +22 |
| `internal/world/scenarios.go` | Mod | `s.defs["no_cache"]`: scope `NoCacheProvider`, targets its band, marker, phases `cache_off` (enter: latency on all providers, cache off), `blip` (enter: fast-fail on the provider, latency kept), `cache_off` (enter: fast-fail cleared, latency kept, post the blip's parked count), exit: cache on, all faults cleared, summary line. `s.defs["no_cache_surge"]`: scope `everyone`, targets the `GlobalSurgeAffectedTop` heaviest (as `global_surge`), phases `cache_off` (enter: latency on all providers, cache off for everyone; post the capacity line after `NoCacheSurgeLead`), `surge` (enter: `gen.SetGlobal(GlobalSurgeMult)`), `cache_off` (enter: `SetGlobal(1)`, post the surge line), exit: cache on, latency cleared, summary line | +65 |
| `internal/metrics/registry.go` | Mod | per-provider KMS call counter per tick (`Audit` already knows the provider from `Idx`), exposed as `ProviderCallsPS(p) float64` for the summary line; not in the snapshot (chart 3 stays by class) | +15 |
| `web/index.html` | Mod | the sixth card (copy below), `data-scenario="no_cache no_cache_surge"`, two Start buttons (as the outage card) and Stop | +7 |
| `web/app.js` | Mod | nothing: cards are generic (`data-start`, `data-stop`, the status line) | 0 |
| `internal/cmek/cmek_test.go` | Mod | `TestPassThrough` (Tests) | +60 |
| `internal/world/world_test.go` | Mod | `no_cache` and `no_cache_surge` rows in `TestScenarioRunner` (three phases each; the band run leaves a tenant outside the band cached, the global run leaves none; both exits restore the cache, the faults and the multiplier) | +30 |
| `internal/metrics/metrics_test.go` | Mod | a `ProviderCallsPS` row | +10 |
| `docs/SPEC.md`, `docs/plan/DESIGN-REFERENCE.md`, `M5.md`, `M6.md`, `COVERAGE.md` | Mod | SPEC › Scenarios gains the card; the amendments row; M5's every-scenario script runs eight starts (the sixth card's two buttons) and expects L2 red during the band run and L1, L2 and L4 red during the global run; M6's scenario guide has six cards | ≈ 12 lines |

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

// SetCache turns the key cache on or off for every tenant of one provider, or of every provider when provider is
// "" (the two no-cache runs' switch).
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

s.defs["no_cache_surge"] = &scenario{
    name: "no_cache_surge", scope: "everyone",
    marker:  "scenario no_cache_surge started: every tenant runs without the key cache for 90 s at p50 20 ms; the global surge (5×) starts at 15 s",
    targets: func() []int { return heaviest(p.GlobalSurgeAffectedTop) },   // the same declared affected set as global_surge
    phases: []phase{
        {name: "cache_off", dur: p.NoCacheSurgeLead,  enter: func() { latencyAll(on); w.SetCache("", false) }},
        {name: "surge",     dur: p.NoCacheSurgeFor,   enter: func() { postCapacity(); w.gen.SetGlobal(p.GlobalSurgeMult) }},
        {name: "cache_off", dur: p.NoCacheSurgeAfter, enter: func() { w.gen.SetGlobal(1); postSurge() }},
    },
    exit: func() { w.SetCache("", true); latencyAll(off); postSummary() },
}
```

Card copy (`web/index.html`, sixth card, Claude drafts, Ben reviews with the page):

> **No key cache.** Tenants run the way a naive integration works: every event is one KMS call to seal and another to deliver, with all three providers at a realistic 20 ms so only the cache differs. **One band + blip (55 s):** gcp tenants lose the cache while the rest keep it. KMS calls/s jump from ~40 to ~240 and the L2 light goes red: that is the point. The affected p99 line climbs 40–100 ms above the healthy one; calls in flight to gcp rise. At 30 s the same 10 s gcp blip the outage card shows as harmless turns all 333 gcp cells orange within a second: `503 key_unavailable`, delivery parked, nothing lost. **Everyone + surge (90 s):** every tenant loses the cache; delivered drops below the 300/s the service was keeping up with a moment ago, because each delivery now waits on a KMS round trip, and the backlog starts to climb. At 15 s the same 5× surge the Global surge card sends arrives: delivered stays pinned near ~215/s against a capacity of 620, KMS calls reach ~1,700/s, calls in flight hit the bulkheads and time out, cells go orange in every band, the backlog hits its cap. L1, L2 and L4 go red together. The cache returns at 90 s.

Summary lines (posted by each run's exit, before the tail). Band run: `no cache (gcp, 55 s): KMS calls gcp 238/s vs aws 13/s · p99 affected 96 ms vs healthy 7 ms · blip: 333 tenants KEY_UNAVAILABLE in 0.5 s`, from `ProviderCallsPS`, the snapshot's p99 pair at the end of the first phase, and `States` one second into the blip. Global run: two lines, `no cache (everyone): delivered 218/s at 300/s offered (capacity 620 with the cache) · KMS calls 590/s` at the end of the lead, and `no cache + 5× surge: delivered 214/s · KMS calls 1,680/s · in flight aws 29 gcp 31 azure 30 · 412 tenants KEY_UNAVAILABLE from timeouts · backlog 20,000 (cap) · with the cache: delivered 620/s, KMS 67/s, backlog under cap` at the surge's end, from the snapshot's tiles and the M3/M4 numbers for the cached comparison (read from the same World's last `global_surge` run when there was one, else the M4 constants).

**amends ARCH** › Scenarios: six cards, eight starts; › cmek › `DecryptKey` "never calls a KMS" gains "unless the tenant is in pass-through"; › Data flow (b) delivery: the worker path is unchanged (a pass-through `DecryptKey` error is released like a lapsed lease).

## Order of work (one lane, Claude; Ben reviews the pull request)

1. **T+0–12 cmek.** `passThrough`, `SetPassThrough`, the two branches, `apply`'s two conditions, `Hot`/`Warm`/`Tick` guards; `TestPassThrough`; `go test -race ./internal/cmek` (the walk, the extras and the concurrent cold callers must pass unchanged: no cached tenant sees a different path).
2. **T+12–24 world and metrics.** Parameters, `SetCache` (band or everyone), the latency helper, the two scenarios with their phases and posted lines, `ProviderCallsPS`; the two runner test rows; `go test -race ./...`.
3. **T+24–28 web.** The card with its two starts; headless check (card renders, either Start disables every other start, status shows the phase and countdown).
4. **T+28–39 acceptance.** Local run at 300/s on `/dev/shm` from the stream: baseline, then `no_cache`: `kms_ps.ok` ≈ 6× baseline and `inflight.gcp` ≥ 5 within 5 s; `p99_ms.affected − p99_ms.healthy` ≥ 40 ms by 20 s with `p99_ms.healthy` within 1.25× its baseline (L1 for the cached two thirds); at blip + 1 s `by_state[2]` ≥ 300 with one aggregated timeline line and `ingest_ps.key_unavailable` > 0; after the blip clears, `by_state[2]` → 0 within 12 s; at exit the summary line; tail `gcp restored: 333 tenants ACTIVE in x s, backlog drained in y s`; `ingest_ps.internal` 0; then `no_cache_surge`: within the lead `tiles.delivered_ps` ≤ 260 at ≈ 300 accepted and `kms_ps.ok` ≥ 500; during the surge `delivered_ps` stays within ± 15 % of its lead value, `kms_ps` total ≥ 1,200, `inflight` ≥ 20 on at least one provider, `by_state[2]` ≥ 100 at some tick with `kms_ps.transient` > 0 (timeouts, not fast-fails), `backlog.total` reaches the cap, `l4.overloaded_ps` > 0; after the exit every tenant returns to ACTIVE within the tail and the backlog drains; a `global_surge` run afterwards reads as in M3 (delivered ≈ 620, KMS ≤ 67/s); 0 unexpected service-log errors; a `provider_blip` afterwards behaves as in M4 (the cache is back).
5. **T+39–40 docs.** SPEC scenarios row, amendments row, M5/M6 notes, README decisions row, TIMELOG row.

## Tests

| Test | File | Asserts |
| --- | --- | --- |
| `TestPassThrough` (new) | cmek_test.go | rig: warm the tenant (one generate); `SetPassThrough(on)` audits a purge and `Info().HotDEKs == 0`; three `EncryptKey` calls make three KMS calls (no singleflight, nothing cached) and each handle seals; `DecryptKey` makes one call per request and opens; a `SetFault(fast-fail)` then `EncryptKey` → `ErrKeyUnavailable` at once and state `KEY_UNAVAILABLE` (never `RIDING_THROUGH`), one `unwrap error` audit with detail `no cache: call failed`; `Tick` after the backoff probes on schedule and the tenant returns to ACTIVE; `Revoke` then `EncryptKey` → `ErrKeyRevoked`, REVOKED, one `REVOKED, 0 DEKs purged` line; `SetPassThrough(off)` → the next `EncryptKey` after a probe serves from the cache with no call |
| `TestScenarioRunner` (rows) | world_test.go | `no_cache` with `Small()` and short phases: `SetCache(false)` applied to the band only (a tenant outside it keeps `HotDEKs`), the three phases in order, exit restores the cache and clears every provider's fault, the summary line is posted, the tail runs. `no_cache_surge`: every tenant loses `HotDEKs`, the generator's global multiplier is `GlobalSurgeMult` during the middle phase and 1 after, the declared targets are the `GlobalSurgeAffectedTop` heaviest, exit restores the cache and clears the latency, both summary lines are posted |
| `ProviderCallsPS` (row) | metrics_test.go | three audits for one provider's tenants and one for another in one tick → 3/0.5 s and 1/0.5 s |
| existing walk, extras, concurrency, scenario runner, hist, transitions, gate | unchanged | green under `-race` |

## Decisions for Ben

**Status: plan drafted 2026-09-23; awaiting Ben's approval before any code (standing process). Each bullet's first option is the recommendation.**

- **Two runs on one card (recommended, Ben's ask 2026-09-23) vs the band run alone.** The band run is the side-by-side (same World, same latency, one third without the cache next to two thirds with it; L1 stays green for the cached two thirds). The global run is the capacity story: a service designed without the cache falls behind its own baseline and then a surge it would otherwise absorb takes it down. Alternative: the band run only (the original plan).
- **Global run: the declared affected set is the `GlobalSurgeAffectedTop` heaviest, as in `global_surge` (recommended) vs everyone.** With everyone a target the healthy p99 series is empty and chart 2 shows one line; with the same 150 as the cached surge, the healthy line is the 850 lighter tenants, it rises too (every tenant pays the round trips), L1 goes red honestly, and the two surge cards' frames compare one to one. Alternative: targets = everyone, L1 shown as n/a for the run.
- **Global run reuses `GlobalSurgeMult` (5×) and a 60 s surge (recommended) vs its own multiplier.** The comparison with the cached surge is the point; a different multiplier would muddy it. The surge is 60 s, not the cached card's 90 s, because the backlog cap is reached in ≈ 15 s without the cache and nothing new happens after that; lever `NoCacheSurgeFor`.
- **Realistic KMS latency on all three providers for the run (recommended) vs only on the pass-through provider.** With the fake's default zero latency, per-request KMS calls cost microseconds and the p99 line would not move; 20 ms is a fair cloud-KMS figure. Applying it to all three providers makes the comparison honest: the cached tenants pay the same latency and do not feel it. Levers: `NoCacheKMSP50/P99`.
- **The tenant in-flight cap stays at 2 during pass-through (recommended) vs raising it.** The top gcp tenants send ≈ 5–15 events/s; at 20 ms per call that is well under two in flight, so `errBusy` 503s should be rare; if the acceptance run shows them, they are a real cost of per-request calls under a cap sized for the cached design and the card can say so. Alternative: lift the cap for pass-through tenants (one line) to keep the message purely about latency and the blip.
- **The L2 light goes red during the run and the card says so (recommended) vs teaching the checker to exempt the targets.** Red is the lesson; M5's every-scenario script expects red L2 for this run and green everywhere else. Alternative: M5's checker excludes pass-through tenants from L2 (a few lines, but then the panel hides the point).
- **Scenario-only switch (recommended) vs a `POST /v1/cache` endpoint.** The card is the demo; an endpoint would be another thing to document and to reset. Alternative: the endpoint, so the video can toggle it by hand.
- **Recovery inside the run keeps the backoff schedule (recommended) vs per-request retries.** Isolates the cache as the one variable (see "deliberately not done"). Alternative: a `naive` flag that also retries per request, showing the hot loop; more dramatic, harder to read, and it spills into the healthy tenants through the worker pool.
- **Phase lengths 30 + 10 + 15 s for the band run and 15 + 60 + 15 s for the global run (recommended).** Long enough for the p99 window (10 ticks = 5 s) to settle before the blip and for the tenants to recover on the probe schedule (≤ 12 s) before the cache returns, short enough for the video; the global lead of 15 s shows the capacity drop before the surge muddies it. Levers: `NoCacheFor`, `NoCacheBlip`, `NoCacheAfter`, `NoCacheSurgeLead`, `NoCacheSurgeFor`, `NoCacheSurgeAfter`.

## Risks

- **Healthy p99 during the run.** The gcp deliveries each hold a worker for an extra ≈ 20 ms; at 300/s that is ≈ 2 worker-seconds per second on an 8-worker pool running at ≈ 47 % (M4's plateau), so the cached tenants' p99 should stay flat, but the pool is a shared resource and a rise would be a real blast-radius finding of the naive design, not a bug. The T+26 run reads `p99_ms.healthy` against its baseline; the lever is `NoCacheKMSP50` (10 ms) before anything structural.
- **The global run's readability.** Timeouts under the bulkhead park tenants in every band, on top of the shedding the admission layer already does at the cap; the grid goes orange in patches and the timeline aggregates per provider. The numbers to read are the tiles (delivered against capacity, KMS calls/s, in flight) and the two posted lines, which the card names; if the orange patches drown the story, the lever is `NoCacheKMSP99` (a tighter tail keeps calls under the timeout and the bulkhead absorbs them) before anything structural. The World itself is fine: ≈ 1,700 fake calls/s is 1,700 timers/s on a 32-deep bulkhead per provider, and the M3 ×10 run already drove admission at this load.
- **Recovery after the global run.** The backlog is at its cap when the cache returns, so the tail's drain takes ≈ 60–70 s at ≈ 300/s of headroom, close to `ScenarioTailMax` (90 s); the card's countdown shows it and the restored line says `not drained` if the box is missed. Lever: `NoCacheSurgeFor`.
- **Timeline volume.** The blip turns ≈ 333 tenants orange in one flush (one aggregated line) and green again over ≈ 12 s of probes (a few aggregated lines): fewer lines than the outage card, not more.
- **Audit rings.** A pass-through tenant writes one `unwrap` audit per event; the 10-entry ring and the per-minute call count in the tenant detail then show exactly that (≈ 300–900 calls/min for a top gcp tenant): the number the card wants a viewer to see.
- **M5 coupling.** The every-scenario script gains two runs (≈ 55 s + tail and ≈ 90 s + a long tail) and their expectations (L2 red in the band run; L1, L2 and L4 red in the global run, green again in the tail); the checker's S4 conservation holds through both (parked rows are counted as queued, shed events were never accepted). M5's plan is amended in its dependencies, nothing in its design.

## Status

**Built 2026-09-23** on `claude/epic-rubin-kgewov`, approved by Ben in chat ("implement it"). Commits 85f829c (pass-through, scenarios, card), ff77495 (peak readings, copy from the first run), then the docs commit. Ben opens the pull request into `main`; M5's plan is amended for the eight starts and the expected red lights. Numbers in `docs/TIMELOG.md` › No key cache row.

**As built, where it differs from the plan above.**

- **The band run's healthy line rises too.** With gcp deliveries each holding a worker for a KMS round trip, the eight-worker pool runs near 80 % and every tenant queues behind it: the healthy p99 read 23–44 ms (baseline 6.6 ms) against the affected 78–108 ms, and fell back to ≈ 3–10 ms during the blip when gcp deliveries stopped. That is the naive design's blast radius crossing tenant boundaries through a shared resource, and the card now says so ("L1 can go red as well"); the plan's "L1 for the cached two thirds" expectation was wrong. The latency lever (10 ms) was not taken: it would have made the global run's lead phase keep up with its baseline (capacity ≈ 314/s at 10 ms) and lost that story. Decision recorded as taken by Claude within the plan's stated mitigation; Ben can flip it (`NoCacheKMSP50`).
- **The blip parks each gcp tenant on its first event, not all at once.** Orange grew 29 → 187 over the 10 s (the tenants that sent an event during the blip; low-rank gcp tenants send less than once per 10 s). The card, the posted line and the expectation say "≈ 190 of 333, each on its first event", not "333 within a second".
- **The global run never reached the bulkheads.** Admission hits the backlog cap ≈ 30 s in and sheds ≈ 1,100 events/s with `503 overloaded`, so KMS calls fall back from a 1,240–1,630/s peak to ≈ 500/s (accepted + delivered), in flight peaks at ≈ 20 of 32, and no call times out: no orange cells in the global run. The picture is delivered pinned at ≈ 200/s against 640, the backlog at its cap and end-to-end p99 in tens of seconds for everyone (Little's law on a queue that grows from the first second). The card and the plan's global paragraph now describe that; the "timeouts park tenants in every band" claim was wrong.
- **Summary lines report peaks**, not the phase's last tick: `metrics.ResetPeaks`/`Peaks` track per-field maxima per phase, because at the end of the surge phase admission had already cut the call rate to ≈ 500/s and the last-tick line under-read it by three times.
- **Recovery after the global run**: 0.5 s to ACTIVE, 57.5 s to drain the cap at ≈ 600/s of delivery (the drain runs at full capacity with the cache back), inside `ScenarioTailMax`.
- `metrics.Reading`/`Last()` (a last-tick reading) exist alongside `ProviderCallsPS`; the plan listed only the latter.
- Review (PR #11, Ben, 2026-09-23, three nits): the pass-through generate goes through the `<t>/generate` singleflight with the need rechecked inside it, so concurrent first (or rotation) events produce one DEK and the joiners use that call's primitive, while the unwrap stays per request; a `summary func()` on `scenario` posts a run's numbers only after a natural end (a stopped run restores everything and posts nothing); the card names the per-tenant cap of two KMS calls in flight as the source of the early `503 key_unavailable` (the cap was kept, as decided).
