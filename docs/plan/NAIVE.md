# Naive mode for the slow-KMS scenario (stretch item X1)

> References: `DESIGN-REFERENCE.md` is the detailed design; "ARCH › section" is shorthand for it. This plan amends it where marked **amends ARCH**. Status at the bottom. Asked by Ben in chat, 2026-09-24: "let's add `Naive-mode switch for the slow-KMS scenario: workers unwrap inline per message, with no lease, cache or bulkheads`" (SPEC › Stretch X1: "The most persuasive moment: one slow KMS takes everyone down, then the switch brings them back").

## Overview

**What it adds.** A second run on the Slow KMS card. The service is switched to the naive design (every seal and every delivery is one inline KMS call: no lease, no DEK cache, no per-provider or per-tenant bulkhead) while azure is slow, and halfway through the run the switch is flipped back with azure still slow. The first half shows one slow provider taking the whole service down; the second half shows the lease, the cache and the bulkheads bringing everyone back while nothing about azure has changed.

**What the viewer sees (expected, to be confirmed by the every-scenario run).**

- **Naive, azure slow (30 s).** azure is a third of the traffic, ≈ 100 events/s. Each azure delivery holds one of the eight shared workers for a KMS round trip at p50 400 ms / p99 3 s against the 500 ms timeout: about 60 % of calls return in ≈ 300 ms on average, the rest time out at 500 ms, so an azure delivery attempt costs ≈ 0.4 worker-seconds and azure alone asks for ≈ 38 worker-seconds per second from a pool of 8. The workers saturate within a second or two; aws and gcp deliveries (instant KMS calls) queue behind the azure ones; delivered falls from ≈ 300/s to well under 100/s, the backlog climbs at ≈ 200+ rows/s, the healthy p99 (aws and gcp tenants, the unaffected two thirds) goes to seconds and **L1 goes red**: this is the blast radius the bulkhead and the cache exist to prevent. azure cells churn orange: a timed-out call parks the tenant (`KEY_UNAVAILABLE`, `503 key_unavailable` at ingest), the next probe succeeds six times in ten and un-parks it, the next delivery may park it again. In flight to azure climbs past the bulkhead's 32 (the tile shows it) because nothing caps it; KMS calls/s ≈ 2 × delivered + accepted, no longer flat (L2's promise broken, chart 3).
- **Leased again, azure still slow (30 s).** The switch flips back at 30 s (a timeline line says so). aws and gcp tenants get a lease on their next event with an instant call and their deliveries never touch the KMS again; the workers free up within a second and delivered climbs back to capacity (≈ 620/s) to drain the backlog. azure tenants take the cold path through the bulkhead: 60 % get a lease within 500 ms and are then cached for 30 s, the rest answer `503 key_unavailable` and retry on the next event, so the band is green within ≈ 10 s except for the yellow flicker the ordinary Slow KMS run shows. Healthy p99 drops back under its limit as the backlog drains; L1 turns green again; in flight to azure pins at 32. The backlog of the naive half (≈ 6–8 k rows) drains in ≈ 15–20 s.
- **Exit.** azure's latency is restored; the tail measures recovery and drain as every card does. The summary line reports the naive half's peaks (delivered floor, backlog peak, healthy p99 peak, azure in flight peak) against the leased half's recovery time.

**What stays the same.** Bulkheads, backoff, probes, the state machine, the sink, admission and the four safety invariants (S4 in particular: a timed-out delivery parks the message, nothing is dropped). The ordinary Slow KMS run is untouched. The no-cache card keeps its two runs: it removes the cache with the bulkheads kept and a realistic 20 ms on every provider; this run removes the bulkheads too and keeps every provider instant except the slow one, so each card isolates one thing.

## Design

- **cmek.** The tenant's pass-through mode (NOCACHE, built) already makes every seal and delivery one direct call. Naive mode is pass-through plus `noBulkhead`: `Manager.SetNaive(id string, on bool)` sets both; `call` skips the per-tenant cap and the provider semaphore for a naive tenant but still counts `m.inflight[provider]`, so the in-flight tile shows the uncapped number. The `KMSTimeout` (500 ms) is kept: the stretch item removes the bulkheads, not the deadline, and without it a slow call would hold a worker for up to 3 s. `SetNaive(id, false)` returns the tenant to the leased path exactly as `SetPassThrough(id, false)` does today (0 hot DEKs, the next event takes the cold path). **amends ARCH** › cmek: one field, one method, one branch in `call`.
- **world.** `World.SetNaive(on bool)` flips every tenant (a design switch is service-wide; the naive service is naive for everyone). Scenario `slow_kms_naive` (scope azure, targets the azure band as `slow_kms`): phase `naive` (`NaiveFor` 30 s: azure slow as in `slow_kms`, `SetNaive(true)`, peaks reset), phase `leased` (`NaiveLeasedFor` 30 s: `SetNaive(false)`, a timeline line `naive mode off: leases, cache and bulkheads back; azure still slow`), exit restores azure's latency and, defensively, `SetNaive(false)`; a `summary` posts the peaks of the naive half and the leased half's recovery. `POST /v1/naive {"on":true|false}` for a manual flip from curl (the Decisions bullet says why it is not a button).
- **web.** The Slow KMS card gets a second Start button ("Start naive") and two sentences of copy; `data-scenario="slow_kms slow_kms_naive"`; no other web change (the lights, tiles and charts already show everything).
- **checker.** Nothing: L1 is judged (a partial scenario), goes red in the naive half and must be green again after the tail; the script allows exactly that.
- **script.** `slow_kms_naive` joins the default order after `slow_kms` (duration 60 s + tail; allowed red L1).

## Files

| Path | New/Mod | Purpose | ~Lines |
| --- | --- | --- | --- |
| `internal/cmek/manager.go`, `fetcher.go`, `types.go` | Mod | `tenant.noBulkhead`, `SetNaive`, the `call` branch | 25 |
| `internal/cmek/manager_test.go` | Mod | a naive tenant with `ProviderInflight = 1` and `TenantInflight = 1`: three concurrent calls all run (in flight reads 3); the same tenant leased again waits on the semaphore | 40 |
| `internal/world/world.go`, `scenarios.go`, `params.go` | Mod | `SetNaive`, the scenario, `NaiveFor`, `NaiveLeasedFor` | 60 |
| `cmd/killswitch/server.go` | Mod | `POST /v1/naive` | 15 |
| `web/index.html` | Mod | second button and copy on the Slow KMS card | 4 |
| `scripts/m5-panel-check.sh` | Mod | the run in the default order, its duration and allowed red | 3 |
| `docs/SPEC.md`, `DESIGN-REFERENCE.md`, `README.md`, `TIMELOG.md` | Mod | X1 marked built in the stretch table; amendments row; the scenario guide's Slow KMS line; a time-log row | 8 |

## Tests and acceptance

- `go test -race ./...` green with the new cmek rows.
- `scripts/m5-panel-check.sh localhost:PORT slow_kms slow_kms_naive` (and the full set once): `slow_kms` unchanged (no reds); `slow_kms_naive` PASS with L1 red only, green after the tail; every light fresh.
- Numbers read from the run into the card copy and this plan's Status: delivered floor and backlog peak in the naive half, azure in flight peak, healthy p99 peak, and the time from the flip to delivered back at capacity.

## Decisions for Ben

Each: recommended · alternative.

- **A scripted flip vs a manual switch.** Recommended: the card's "Start naive" runs the two halves on a timer (30 s naive, 30 s leased, azure slow throughout), so the video and the every-scenario script get the same story every time, and `POST /v1/naive` exists for a manual flip from curl. Alternative: a "Naive mode" toggle button on the card the presenter clicks during an ordinary Slow KMS run (more theatrical, but the script cannot judge it and a forgotten toggle leaves the shared World naive for the next viewer).
- **Service-wide vs azure-only.** Recommended: naive for every tenant, because the switch stands for a design and the collapse comes from the shared workers either way. Alternative: only the azure band is naive (the same collapse; the aws and gcp tenants keep their leases, which makes the "one slow provider takes everyone down" line slightly less honest).
- **Other providers' latency.** Recommended: unchanged (instant, as in the ordinary Slow KMS run), so the only variable against `slow_kms` is the naive switch. Alternative: the no-cache card's realistic 20 ms on every provider (a deeper collapse, but two variables at once).
- **Timeout kept at 500 ms.** Recommended (the item says no bulkheads, not no deadline). Alternative: also lift the timeout in naive mode, so a slow call holds a worker up to 3 s; the collapse is faster and the parked count lower, and a stuck worker is the picture.
- **Phase lengths 30 s + 30 s.** Recommended. Alternative: 20 s + 40 s if the naive half's backlog drains too slowly to read.

## Status

**Built 2026-09-24** on `claude/epic-rubin-kgewov`, approved by Ben in chat ("Scripted flip sgtm, naive for everyone, plan LGTM"; the other three decisions as recommended). Commit 4266af6 (code), then the docs commit. Suite green under `-race`; `scripts/m5-panel-check.sh localhost:PORT slow_kms slow_kms_naive`: both PASS, `slow_kms` with no red and `slow_kms_naive` with L1 red only (129 frames), every light fresh and green after the tail.

**As built and as measured (one captured run on this box, tmpfs).**

- **Naive half.** Within two seconds of the switch, delivered fell from ≈ 300/s to 8–26/s (with short bursts to ≈ 500/s every few seconds as a batch of azure calls resolved together), ≈ 50 azure tenants parked at a time (`503 key_unavailable` 40–110/s at ingest), azure in flight peaked at 45 against the bulkhead's 32, KMS calls ran at ≈ 300/s instead of ≈ 50, the backlog climbed to 3,048 and the healthy p99 (aws and gcp) rose to 24.8 s at the flip. L1 went red 9 s in on the p99 (`p99 6625ms > 31ms for 3 s`) and stayed red on rejections from 18 s: the heaviest aws and gcp tenants filled their per-tenant backlog cap (500 rows) because nothing was delivered and got `503 backlog_full` at 20–58/s, 580 rejections in all. The plan predicted the collapse and the p99; the `backlog_full` rejections on healthy tenants are the blast radius reaching ingest, and the card now says so.
- **Leased half.** One tick after the flip, delivered was at 588/s and then 604–624/s (capacity) while the backlog drained from 3,048 to under 40 in ≈ 9 s; KMS calls fell to 20–80/s; azure in flight returned to ≤ 32 and then single digits as leases filled. The healthy p99 read 25–36 s for the next 9 s (the window still held the naive half's late deliveries) and then 5 ms; the affected p99 (azure, its parked messages delivered late) read ≈ 32–37 s until its window cleared. L1 stayed red for the rest of the run because its rejection count is cumulative since the scenario started (as designed: a rejection rate of 0 is the promise) and turned green at the scenario's end; the script requires exactly that.
- **Timeline lines** as built: at the flip, `naive mode off: leases, cache and bulkheads back, azure still slow · the naive half ended at delivered 18/s, backlog 3048, healthy p99 24.8 s, KMS calls 196/s; azure in flight peaked at 45 (bulkhead 32)`; the summary, `naive vs leased (azure slow throughout): delivered 18/s at the flip → 624/s peak draining with the leases back · backlog peaked at 3048 · healthy p99 24.8 s naive vs 36575 ms leased`; the tail, `azure restored: 333 tenants ACTIVE in 9.6 s, backlog drained in 0.1 s`. The summary's "leased" p99 is the leased half's peak, which is the drain's late deliveries, not the steady state; the wording is kept because the number is what the tile showed.
- `TestNaiveSkipsBulkheads` (cmek): six concurrent seals on a naive tenant with both caps at 2 are all inside the KMS at once and in flight reads six; the same six seals with the cache back share one cold fetch (the singleflight), so exactly one call is inside the KMS. That second half is a stronger contrast than the plan's "held to the caps again": on the cached design the caps never even bind for one tenant's concurrent events.
