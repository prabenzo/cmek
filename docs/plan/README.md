# Killswitch implementation plans (M0–M6)

These are the build plans for the seven milestones in `docs/SPEC.md › Plan and cut lines`. Nothing is built until Ben approves a milestone's plan. Approval is a tick in the checklist below plus a note of any decision where the alternative is chosen.

## The short version

- **Every spec item has a milestone** (`COVERAGE.md`), the seven clocks chain 0:00 → 4:25 and sum to 3:40 build + 0:45 rationale, with the 0:20 buffer untouched on paper.
- **The expected path takes three of the spec's own cut lines.** M1 is honestly ≈ 1:11, not 1:05, so the tenant-detail endpoint is the expected M1 cut (rebuilt in M4 when hover/pin are attempted) and M2 starts with `TenantInflight = 0` (its cut). M5 expects to ship four lights (S1–S4) and land L1/L4 in the 20-minute buffer with a redeploy. Each plan says at which minute and on which signal its cut is taken, so no cut is a surprise on the day.
- **Ben's decisions are per milestone**, about 50 in all (indexed in `COVERAGE.md § 11`); `ARCHITECTURE.md` lists only the architectural ones and points at the milestone that decides each. Each bullet gives a recommended option, the alternative, and what changes in the plan if the alternative is chosen. Only M0's three (deploy path, cross-compiling builder stage, tick burn) are needed before the clock starts; M1's first one (nonce source) is needed by 0:22.
- **Fast path to approval:** reply "approve M0–M6 as recommended" or list the bullets you change; a changed bullet's plan delta is already written.

## How to review (about an hour)

1. Read `ARCHITECTURE.md` (ten minutes): the components, the data path, the key-lease mechanism, the isolation and admission model, the trust boundary, the World lifecycle on Cloud Run, and the handful of architectural decisions, each pointing at the milestone plan that carries its detailed form.
2. Read `ARCH-NOTES.md` (nine facts and corrections the plans cite by number, such as the exact Cloud Run command and the Go 1.26 requirement). `DESIGN-REFERENCE.md` is the detailed design every plan cites (signatures, SQL, parameters with demo defaults, concurrency rules, twenty design questions answered); read it on demand, when a plan cites a section. Its last two appendices are the planning record. `CONTEXT.md` is the brief the planners worked from.
3. Read `M0.md` through `M6.md` in order. Each follows one template: a plain-English overview (what the milestone accomplishes, what it needs from earlier milestones, what later ones take from it), then goal, cut line, detailed dependencies, files with line counts and owner, interfaces, order of work with a running clock in two lanes ([C] Claude in the session, [B] Ben on his laptop), tests, acceptance check, decisions for Ben, risks, and a review log of the findings that shaped it. Library-, struct- and timeout-level questions live here, not in the architecture document.
4. Use `COVERAGE.md` to check nothing was dropped and to see every decision in one table.

## Approval checklist

| Milestone | Clock | Plan | Approved by Ben | Decisions changed |
| --- | --- | --- | --- | --- |
| M0 Skeleton and deploy | 0:00–0:20 | [M0.md](M0.md) | [x] 2026-09-21, PR #1 review | all three as recommended |
| M1 Data path | 0:20–1:05 (expected ≈ 1:11) | [M1.md](M1.md) | [ ] | |
| M2 CMEK core | 1:05–1:50 | [M2.md](M2.md) | [ ] | |
| M3 Admission and surges | 1:50–2:20 | [M3.md](M3.md) | [ ] | |
| M4 UI and scenarios | 2:20–3:15 | [M4.md](M4.md) | [ ] | |
| M5 Checker and hardening | 3:15–3:40 | [M5.md](M5.md) | [ ] | |
| M6 Rationale | 3:40–4:25 | [M6.md](M6.md) | [ ] | |

## Decisions recorded so far

| Decision | Where | Ben's call | Recorded |
| --- | --- | --- | --- |
| Scheduler class | ARCHITECTURE.md, M3 | as recommended (interleaved two-class ring) | PR #1 review, 2026-09-21 |
| DEK rotation during ride-through | ARCHITECTURE.md, M2 | as recommended (keep sealing under the exhausted DEK; "look good for the demo, adjust from there") | PR #1 review, 2026-09-21 |
| Provider bulkhead full | ARCHITECTURE.md, M2 | as recommended (wait within the call deadline) | PR #1 review, 2026-09-21 |
| Stale-OK guard | ARCHITECTURE.md, M2 | as recommended (keep the guard) | PR #1 review, 2026-09-21 |
| M0: deploy path, cross-compiling builder stage, tick burn | M0 | all as recommended | PR #1 review, 2026-09-21 |

## Working asynchronously

Ben reviewed the time commitments as fine but will work asynchronously, not to the proposed timing in real time. The plans stay as written; read them as follows:

- **The clock is a budget of working minutes per lane, not wall time.** Every "0:07", "T+14" and trigger minute is measured on that lane's own working clock, from the moment the lane's owner picks the milestone up. `docs/TIMELOG.md` records working minutes per lane and per milestone, which is what the assignment's "time spent" needs.
- **Hand-offs become pushed-and-waiting states.** Where a plan has Claude and Ben working in parallel, Claude's lane runs in a session to its next push point and stops; Ben's lane runs whenever he picks it up (pull, his files, reviews, deploys) and ends with a push; the next Claude session starts by pulling. Nothing in either lane waits idle for the other in real time.
- **Cut lines and fallback triggers still apply**, judged on the lane's working clock and on the same signals (a build not green, a measurement over its threshold), never on the calendar.
- **Acceptance checks that need both hands** (Ben's laptop for browser, Docker and gcloud; the session for curl and Go) are split as the plans already say; the live-URL checks simply happen at Ben's next pickup.

## Ground rules carried into every plan

- **Budget.** 3:40 build, 0:45 rationale, 0:20 buffer (D5, D8). Each plan stays inside its box, names the minute and the signal at which its cut line is taken, and starts its clock from the actual start time recorded in `docs/TIMELOG.md` (created in M0, one row per box, filled at every boundary).
- **Ownership.** Ben writes or line-reviews every line of `internal/cmek`, drafts the two-class scheduler pass and the checker's data statements, and reviews the queue's claim/release code, the KMS interface, ingest, holder and world wiring, and the checker's verdict rules. Claude drafts the rest at Ben's direction. Every file header carries its owner.
- **Two lanes, one repo.** Claude pushes at fixed minutes; Ben pulls, writes his files and review fixes, and pushes; one writer per file at a time. The plans say `main`; the branch convention during the build is Ben's call (these plans themselves are on `claude/epic-rubin-kgewov`).
- **Machines.** `go build`, `go vet`, `go test` and curl checks run in the session. `docker build`, `docker push`, `gcloud run deploy`, the browser checks, the recording and the transcript exports are Ben's on his laptop; the planning session had no Docker daemon, no gcloud and no browser.
- **Toolchain.** `go 1.26` in go.mod and `golang:1.26` in the Dockerfile: the pinned `golang.org/x/sync v0.23.0` and `golang.org/x/time v0.16.0` require it (verified by a build in the session; `modernc.org/sqlite v1.59.0`, SQLite 3.53.4, WAL mode, `RETURNING` and `json_each` all verified too).
- **Naming.** "lease" means the key lease only; message rows are "claimed" with `claimed_until`. No package-level mutable state; every time constant and load setting is a `world.Params` field with its demo default.

## Where the plans came from

The architecture came from three independent design passes (boundaries-first, invariant-first, budget-first), scored by three judges with different lenses, synthesized from the winner with grafts from the others, then attacked by three adversarial reviewers (safety and concurrency, spec fidelity, buildability) whose 42 findings were each accepted or rejected with a reason. Each milestone plan was drafted, checked by three verifiers (fidelity, time realism, test and acceptance rigor), revised, then checked across milestones for coverage and consistency; the fixes are the `CR-n` rows in each review log. Two facts were verified by running things in the session rather than by reading: the Go 1.26 requirement, and the SQLite driver features the queue depends on.
