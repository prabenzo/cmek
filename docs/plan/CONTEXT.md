# Shared brief for planning agents (read fully before doing anything)

You are helping produce an IMPLEMENTATION PLAN (not code) for "Killswitch", Ben's take-home project.
Nothing is to be built yet. Ben must approve each milestone's plan before any build work starts.

## Inputs (read all three, in this order)
1. SPEC.md            — the spec, source of truth. Every number, name, endpoint, state, invariant and cut line in it is binding.
2. P0_EXPLAINED.md    — plain-language drill-down of every P0 term. Binding where it adds detail (e.g. at-least-once via claim timeout, ring-of-tenants scheduler, the lease timeline table, the "lease" vs "claimed" naming rule, checker runs once a second, traffic stops with 0 viewers).
3. HANDOFF.md         — decisions already settled with Ben and the assignment constraints. Do not reopen settled decisions. Where the spec and P0_EXPLAINED disagree on a number, the spec's parameter tables win (e.g. tenant backlog cap is 500; P0_EXPLAINED's "for example 5,000" is illustrative).

## Environment facts (binding)
- Repo: github.com/prabenzo/cmek, currently only README.md and docs/SPEC.md. Module path to use: `github.com/prabenzo/cmek`. Binary name: `killswitch` (cmd/killswitch).
- Build machine in this session: Go 1.24.7 linux/amd64, Docker present, gcloud NOT present. Ben's laptop is assumed to have gcloud and Docker; Cloud Run deploys happen from Ben's machine (or a GitHub Actions workflow Ben sets up), never from this session.
- Libraries allowed (spec): stdlib net/http, crypto/aes, crypto/cipher; golang.org/x/sync/singleflight; golang.org/x/time/rate; modernc.org/sqlite. uPlot for charts (vendored as a single JS + CSS file under web/, no npm).
- `testing/synctest` is stretch X4 only; P0 tests use an injected clock.
- Package layout from the spec (binding):
  cmd/killswitch/     HTTP server, embeds the UI, owns the World
  internal/world/     wires everything; parameters; Reset
  internal/cmek/      lease, DEK cache, classifier, key fetcher, envelope (the core; Ben owns it; ~500 lines)
  internal/kms/       KMS interface, fake providers, fault injection, ground truth
  internal/queue/     SQLite store, fair scheduler, workers
  internal/admit/     token buckets, backlog caps
  internal/traffic/   load generator, fake sink
  internal/check/     invariant checker
  internal/metrics/   aggregates, histograms, SSE snapshot
  web/                index.html, app.js, uPlot

## Non-negotiable design rules (from spec + decisions)
- No package-level mutable state anywhere. Everything hangs off one World struct tied to one context. Two Worlds must be able to coexist in one process (needed by tests and stretch X2).
- Only the key fetcher (internal/cmek) ever calls a KMS. Workers never call a KMS and never block on one.
- The word "lease" is reserved for the key lease. Message claims use `claimed` / `claimed_until`.
- Every time constant and load setting is a World parameter (a Params struct with demo defaults), never a constant buried in a package.
- The invariant checker sits outside the service's trust boundary: it reads stored rows, the sink's delivery record and the fake KMS's ground truth; the service must not be able to influence its verdicts.
- Lease validity is measured from when the renewal request was SENT; the lease expires locally 1 s early.
- Budget: 3:40 of build across M0–M6 with the clock and cut lines in the spec's "Plan and cut lines" table. Plans must fit those boxes. Claude (an AI pair) drafts most non-core code at Ben's direction; Ben writes or line-reviews internal/cmek himself. Mark ownership per file. (Superseded 2026-09-22: Claude writes all code and tests; Ben reviews. ARCH-NOTES 9.)
- Deliverables to keep in view: deployed URL, GitHub repo, ~5 min video + short written doc (M6), AI transcripts, and a note of time spent (keep docs/TIMELOG.md: planned vs actual per milestone, created in M0).

## Output style (binding for every agent that writes plan text)
- Markdown. Terse. Tables for files/tests; Go code blocks for signatures and types; numbered lists for order of work. No motivational prose, no restating the spec at length. Reference spec items by their IDs (S1–S4, L1–L4, D1–D9, X1–X4, M0–M6).
- Go signatures must be concrete and compilable-looking (package, exported names, parameter and return types). One-line doc comment per exported identifier. Prefer small interfaces.
- Give approximate line counts per file so the budget can be sanity-checked.
- Where a real design choice exists, state the recommended option in one line and the alternative in one line, tagged "Decision for Ben". Do not leave it open-ended.
- Every plan must be honest about what is hard; do not hand-wave concurrency, SQL statements, or time math.

## Milestone plan template (each milestone file must follow this exactly)
```
# M<k> <name> — <clock from spec> (<duration>)

**Goal.** one or two sentences.
**Cut line (from spec).** what gets dropped if it overruns.
**Depends on.** milestones / interfaces it consumes.

## Files
| Path | New/Mod | Purpose | ~Lines | Owner (Ben/Claude) |

## Interfaces and types
Go code blocks: the exported surface this milestone adds or changes, with one-line doc comments.

## Order of work
1. step (≈ minutes) ...  with a running clock that stays inside the box.

## Tests
| Test | File | Asserts |
(unit tests only where the spec asks for them or a bug would silently break an invariant; keep proportionate)

## Acceptance check
Concrete commands / clicks and the exact observations that mean "done". For M0 this includes the Cloud Run SSE ticker check.

## Decisions for Ben
Bullets: each a recommended option plus alternative, one line each.

## Risks specific to this milestone
Bullets with the mitigation.
```

## Architecture doc template (for the architecture stage)
```
# Architecture and conventions
## Package dependency graph (mermaid, arrows = imports; must be acyclic; internal/cmek imports only kms + stdlib + x/sync)
## World: struct, Params (every spec parameter with its demo default), lifecycle (New, Run, Reset, viewer count, idle pause, idle rebuild), goroutine inventory (name, rate, owner)
## Shared types and error sentinels (ids, states, classes, ingest outcomes → HTTP code + JSON body + Retry-After)
## Data flow: exact call sequences for (a) ingest, (b) delivery, (c) key fetch / lease renewal / probes / expiry, (d) fault injection, (e) checker reads, (f) SSE snapshot
## SQL schema and statements (CREATE TABLE, indexes, insert, claim, ack, release, expire, counts) and connection strategy (writer/reader, pragmas)
## Per-package exported surface (Go signatures, one-line docs) with ~line budget per package
## Concurrency and consistency rules (which mutex owns which counters; how the S4 snapshot is consistent; lease check at decrypt time)
## Conventions (naming, no globals, clock injection, errors, logging, tests)
## Answers to the hard questions (numbered, one paragraph each)
## Decisions for Ben
```

## Hard questions every architecture proposal must answer explicitly (numbered answers, one short paragraph each)
1. Where does each per-tenant counter live (accepted, delivered, queued ready/claimed, expired, dead, rejected-by-reason) and how does the S4 conservation check get a snapshot in which accepted = delivered + queued + expired + dead holds exactly, not just eventually?
2. On the delivery path, when is lease validity checked (claim time, decrypt time, or both) and what happens to a claimed batch whose lease expires or is purged before decrypt (must satisfy S3 and S4: no decrypt after the bound, no burned attempt)?
3. A cold tenant whose lease lapsed through idleness gets a transient failure on its synchronous fetch at ingest: what state, what HTTP response, what transition? The spec's state diagram has no ACTIVE→KEY_UNAVAILABLE edge; propose the smallest consistent rule.
4. Who drives lease expiry and probes for tenants with no traffic and no backlog (ticker vs lazy), at what rate, and how does that stay cheap for 1,000 tenants?
5. How does the scheduler know which tenants have ready backlog without scanning SQL? Exact claim statement, batch size, claim timeout, and what a worker does on success/poison/lease-gone.
6. SQLite: connection strategy (single writer, reader pool), pragmas (journal_mode, synchronous), and the expected write rate at 1,500 events/s ingest + claims + acks. Is per-event synchronous insert acceptable, or must inserts be batched, and what does 202 then mean?
7. Fair-share shedding for the global backlog cap: give a definition computable in O(1) per ingest with maintained counters, and show with the spec's numbers (1,000 Zipf tenants, 300→1,500 events/s, capacity ≈ 640/s, tenant cap 500, global cap 20,000) which tenants get 429 backlog_full vs 503 overloaded during a 60 s global surge.
8. How is the "affected" tenant set defined for the p99 healthy-vs-affected chart and the L1 light (scenario-declared target set vs state-derived), and what is the baseline for "1.25x of baseline"?
9. Metrics: how p99 is computed cheaply at ~300–1,500 samples/s (fixed-bucket histogram vs sample buffer), what one SSE snapshot JSON contains, and how the 1,000-byte grid state array is encoded.
10. World lifecycle on Cloud Run request-based billing: viewer counting on /v1/stream, pause traffic at 0 viewers, rebuild after >10 s idle, POST /v1/reset, and how main.go holds "the current World" without globals (a holder struct owned by main). What exactly does the M0 ticker check measure and how?
11. Where the audit ring (per tenant, last N entries) lives and how GET /v1/tenants/{id} and the grid's click-to-pin detail read it.
12. Ingest outcome sentinels (ErrRateLimited, ErrBacklogFull, ErrOverloaded, ErrKeyUnavailable, ErrKeyRevoked) and the exact HTTP status + JSON body + Retry-After mapping; where the load generator calls in (same function as the HTTP handler, per P0_EXPLAINED).
13. Where the KMS ground-truth log lives and how the checker reads it while the service cannot.
14. DEK lifecycle: GenerateDataKey on first use and on rotation (10,000 messages or 10 min); multiple DEKs per tenant in cache and in the deks table; what "active DEK" means for encrypt vs decrypt; what renewal unwraps.
15. Line budget per package, with internal/cmek held near 500 lines; list what must NOT go into internal/cmek to keep it there.
16. Testability: clock injection in internal/cmek; how a unit test builds a Lease/Manager with a fake KMS and a fake clock without HTTP or SQLite; how a test builds a whole World.
17. Bulkheads: how the per-provider (32) and per-tenant (2) caps and the 4-per-tenant ingest waiter cap are implemented (semaphores/buffered channels), and what a caller gets when a cap is full (fast 503 vs wait).
18. Backoff: exact schedule (0.5 s doubling to 8 s, ±50% jitter), single probe at a time per tenant, and the REVOKED re-probe every 5 s; where randomness comes from (World-owned *rand.Rand, seeded from Params for reproducibility).
19. Scenarios: how a scripted scenario runs (a goroutine with phases; start/stop; countdown surfaced in the snapshot), how it declares its target tenant set, and how the five P0 scenarios map to the fault and traffic APIs.
20. SSE mechanics: 2 Hz tick, per-viewer channel with drop-on-slow policy, reconnect behavior when Cloud Run's 60-minute request timeout ends the stream, and the "starting state" the page shows before the first snapshot.
