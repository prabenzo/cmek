# Key-fetch modes on the Slow KMS card (replaces the scripted naive run)

> References: `DESIGN-REFERENCE.md` is the detailed design; "ARCH › section" is shorthand for it. This plan amends it where marked **amends ARCH**. Status at the bottom. Asked by Ben in chat, 2026-09-24: the card should let him flip the service by hand between the naive design, a synchronous-fetch design and the asynchronous one that ships, while azure is slow, and Reset world puts everything back.

## Overview

**What changes.** The Slow KMS card keeps its Start and Stop (azure slow for 60 s, unchanged) and gains three mode buttons that flip the whole service's key-fetch design live, at any time, scenario or not: **Naive** (no lease, no cache, no bulkheads: every seal and delivery is one inline KMS call), **Sync fetch** (the cache and the lease are kept, the bulkheads too, but nothing renews in the background: the worker that finds a lease due does the fetch inline, blocking under the 500 ms timeout, and a parked tenant is re-probed the same way) and **Async fetch** (the design that ships: the renewal is kicked into the background at the soft TTL and workers never wait). The active mode is shown on the card and in the page header, each flip posts a timeline line and resets the peak readings, and a new World starts in Async, so Reset world is the way back to the beginning. The scripted `slow_kms_naive` run and its button go away: the presenter drives the ladder.

**The demo as Ben will drive it.** Click Naive, click Start: within seconds delivered collapses from ≈ 300/s to ≈ 20/s, the healthy p99 climbs to tens of seconds, KMS calls run at ≈ 300/s, azure in flight passes 32 and the heaviest aws and gcp tenants get `503 backlog_full` (the measured naive run, NAIVE.md › Status). Click Sync fetch: KMS calls fall back to tens per second and the in-flight tile drops under the bulkhead, the cache and the call economy are back, and the collapse stays, because ≈ 22 azure renewals a second (333 tenants, 15 s soft TTL) each hold a worker for 0.3–0.5 s and riding-through tenants re-probe on the backoff cadence on top: the eight workers spend their time waiting on azure and aws and gcp deliveries starve behind them (the expected picture; the measured one goes into this plan's Status, with a longer lease for that mode as the lever if a visible gradation is wanted). Click Async fetch: the same renewals move to background goroutines under the bulkhead, the workers are free within a second, delivered goes to capacity to drain the backlog and the healthy line comes back down while azure is still slow. Stop or let the run end; Reset world for a clean start.

**What stays the same.** The other cards, the checker, the lights (L1 goes red in Naive and Sync while a partial scenario runs, and turns green after the tail, as today), the no-cache card (its pass-through mode is unchanged and is not one of the three buttons: it keeps the bulkheads and the realistic 20 ms and isolates the cache; the Naive button is that plus no bulkheads, as built in NAIVE.md).

## Design

- **cmek.** One per-tenant `fetch` mode replaces the two booleans: `FetchAsync` (default), `FetchSync`, `FetchPassThrough` (the no-cache card's `SetPassThrough`, unchanged in behaviour) and `FetchNaive`; `Manager.SetFetch(id, mode)`; `SetPassThrough`/`SetNaive` become thin wrappers so nothing else moves. **Sync** means: `EncryptKey`'s hot path and `Hot()` do not kick the background renewal; `DecryptKey` (the worker's call) renews inline when the lease is soft-due and no renewal is running (set `probing`, release `t.mu`, run the existing `probe`, retake the lock, continue with the normal checks), and probes inline when a RIDING_THROUGH, KEY_UNAVAILABLE or REVOKED tenant's `nextProbeAt` is due; `Hot()` returns true for a parked sync tenant whose probe is due, so the scheduler dispatches it and the worker's `DecryptKey` is the probe (a failed probe answers `ErrLeaseExpired` and the message is released as today); `Tick` skips the probe spawn for sync tenants. The ingest cold path (no usable lease) blocks and renews as today in every mode. **amends ARCH** › cmek: the mode field, `SetFetch`, the inline branch in `DecryptKey`, the `Hot` rule, the `Tick` skip.
- **world.** `World.SetFetch(mode)` for every tenant with a timeline line (`key fetch: sync (cache and lease kept; the worker fetches inline)`) and `metrics.ResetPeaks()`; `Params.KeyFetch` default `async`; the snapshot gains `key_fetch: "async"|"sync"|"naive"` (the no-cache runs report `async`: their pass-through is a scenario, not a mode). `slow_kms_naive` and `NaiveFor`/`NaiveLeasedFor` are removed; `POST /v1/naive` becomes `POST /v1/keyfetch {"mode":"naive"|"sync"|"async"}` (400 on another value).
- **web.** The Slow KMS card: three mode buttons in a second row, the active one highlighted from `key_fetch`, hover text per mode; the header banner shows the mode when it is not `async` (`mode: naive`). Card copy: two sentences per mode.
- **checker.** Nothing.
- **script.** `slow_kms_naive` is replaced by two mode-driven runs of `slow_kms`: `slow_kms@naive` and `slow_kms@sync` (the script sets the mode through the endpoint before Start, flips back to `async` at 30 s and asserts as before: L1 red allowed, every light green after the tail, `async` restored at the end).

## Files

| Path | New/Mod | Purpose | ~Lines |
| --- | --- | --- | --- |
| `internal/cmek/lease.go`, `manager.go`, `fetcher.go` | Mod | `fetch` mode, `SetFetch`, the sync branch in `DecryptKey`, `Hot`, `Tick`, the `call` bulkhead test on the mode | 70 |
| `internal/cmek/cmek_test.go` | Mod | `TestSyncFetch`: a sync tenant's soft-due lease is renewed by `DecryptKey` itself (one call, blocking, the worker's handle valid after), never kicked from `EncryptKey` or `Hot`; a parked sync tenant is Hot when its probe is due and the worker's `DecryptKey` probes it back to ACTIVE; `TestNaiveSkipsBulkheads` adjusted to `SetFetch` | 60 |
| `internal/world/world.go`, `scenarios.go`, `params.go`, `internal/metrics/registry.go` | Mod | `SetFetch`, snapshot key, the scripted run and its Params removed | 40 |
| `cmd/killswitch/server.go` | Mod | `POST /v1/keyfetch` | 15 |
| `web/index.html`, `app.js` | Mod | mode buttons, highlight, banner, copy | 30 |
| `scripts/m5-panel-check.sh` | Mod | the two mode-driven runs | 15 |
| `docs/SPEC.md`, `DESIGN-REFERENCE.md`, `NAIVE.md`, `README.md`, `TIMELOG.md` | Mod | X1 row, amendments row, NAIVE.md status note (superseded by the buttons), scenario guide, time-log row | 10 |

## Tests and acceptance

- `go test -race ./...` green with the new cmek rows.
- The script: `slow_kms` unchanged; `slow_kms@naive` and `slow_kms@sync` PASS with L1 red only and green after the tail; the full set once.
- Headless check of the buttons: the mode flips, the highlight follows `key_fetch`, the timeline line posts, Reset world returns to `async`.
- Measured numbers for the sync half into this plan's Status and the card copy.

## Decisions for Ben

Each: recommended · alternative.

- **Modes as service-wide buttons, not per band.** Recommended (a design switch is service-wide; Ben's ask). Alternative: per-provider buttons.
- **Drop the scripted naive run.** Recommended: the buttons replace it and the script keeps the coverage through the endpoint. Alternative: keep `slow_kms_naive` as a fourth button for a hands-free take.
- **Sync mode's lease length.** Recommended: unchanged (30 s, soft 15 s), so the only variable between the buttons is where the fetch runs; if the sync collapse is as complete as naive's, the plan's Status says so and names the lever (`Lease` 60 s for that mode) for a second decision. Alternative: give sync mode a longer lease from the start.
- **Ingest in sync mode.** Recommended: the ingest hot path serves from the cache and never fetches; only the cold path (no usable lease) blocks, as today. Alternative: ingest also renews inline at the soft TTL (more collapse, mixes two stories).

## Status

**Plan drafted 2026-09-24; awaiting Ben's approval before any code (standing process). Each bullet's first option is the recommendation.**
