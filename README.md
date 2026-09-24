# Killswitch

A CMEK-compliant multi-tenant webhook event queue. Each tenant owns the key that protects its data: revoke it and the provider can no longer read anything of that tenant's, within a published bound of 30 seconds. One tenant's KMS trouble, revocation or traffic surge changes nothing for the other 999, because every message is sealed under a per-tenant data key that is leased and cached, so the KMS is off the request path and a KMS blip is ridden through instead of felt. The invariants that say so are judged live, from outside the service, on the page.

Live URL: _set by Ben after the M5 deploy_. Spec: [docs/SPEC.md](docs/SPEC.md). Plans and decisions: [docs/plan/](docs/plan/README.md). Time log: [docs/TIMELOG.md](docs/TIMELOG.md).

## What to click

The page is one shared live system: 1,000 tenants across three simulated KMS providers (aws, gcp, azure) at about 300 events/s, a grid of one cell per tenant, four tiles, four charts, the invariant panel and a timeline. Each card injects a fault or a surge and says what to watch; every card has a Stop, and Reset world rebuilds everything under a new id. Traffic runs only while a tab is open.

- **Provider outage.** *Blip (10 s)*: gcp fast-fails; up to a third of the grid turns yellow (RIDING_THROUGH on a cached lease), a few cold cells go orange with one `503 key_unavailable`, everything is green within about 12 s of the clear and no chart moves. *Outage (60 s)*: yellow spreads with no customer impact, then cells turn orange as leases lapse at staggered times and those tenants' events park; aws and azure stay flat, calls to gcp decay through backoff, and on restore the parked messages deliver. Nothing is lost.
- **Key revocation.** One high-traffic tenant's key is disabled. Its cell turns purple within 30 s (typically 15), ingest answers `403 key_revoked`, the timeline shows the service's `REVOKED, n DEKs purged` line and the checker's `revoked at … (ground truth), detected in x s` line, and no message of that tenant is decrypted after the bound. Click Restore: the re-probe sees it within 5 s and the parked messages deliver.
- **Slow KMS.** azure's latency rises to p50 400 ms, p99 3 s against a 500 ms timeout. azure cells flicker yellow, cold ones see slow ingest and a few 503s, in-flight calls to azure pin at the bulkhead, and the healthy p99 line stays flat. The three *key fetch* buttons switch the whole service's design live, before or during the run. *Naive* (no lease, cache or bulkheads): delivered collapses to ~20/s for everyone, KMS calls run near 300/s, the healthy p99 reaches ~25 s, heavy aws and gcp tenants get `503 backlog_full`, L1 goes red. *Sync fetch* (cache and lease kept, the worker fetches inline): calls fall back to tens per second and throughput mostly holds, but every tenant's latency is coupled to azure, with the healthy p99 past 10 s. *Async fetch* (what ships): the workers are free within a second, delivered goes to capacity and the backlog drains with azure still slow. Reset world starts over in Async.
- **Tenant surge.** One top-20 tenant sends 100× for 60 s. Its cell stays green with a ring; above its limit it gets `429 rate_limited`; its KMS calls stay at most one per 15 s; the other tenants do not move.
- **Global surge.** Every tenant sends 5× for 90 s, about 1,500 events/s against a capacity near 640. Delivered holds at capacity, the heaviest tenants are held to their fair share with 429s and near the end the heaviest few dozen are shed with `503 overloaded`, the backlog levels off under its cap and drains in about a minute, KMS calls stay under about 67/s. L1 is not judged here (every tenant is offered load); L4 is.
- **No key cache.** The naive integration, for contrast: no lease, no cache, two KMS calls per event. *One band + blip*: gcp loses the cache, KMS calls jump from ~30 to ~190/s, the gcp p99 climbs and the healthy line rises with it because the delivery workers are shared, and the same 10 s blip the outage card rides through parks about 190 tenants. *Everyone + surge*: delivered drops to ~200/s before any surge, then the 5× surge pins it there, the backlog hits its cap and admission sheds ~1,100 events/s; L4 goes red for the run and green again after.

Cards can be hidden one at a time (the × in each corner) or as a column (Hide cards); a hidden card's scenario keeps running and the timeline still reports it. The panel's six lights are judged once a second by a checker that reads only the stored rows, the sink's delivery record and the fake KMS's ground truth, never the service's own counters. One detail worth knowing when watching a revocation: a KMS reply that was sent before the key was disabled can arrive after it, so a late OK is ignored whenever it predates the deny and a revoked tenant can never be un-parked by a stale answer.

## Run it

```
go run ./cmd/killswitch            # http://localhost:8080; the page starts traffic when a tab opens
go test -race ./...                # every package, under the race detector
scripts/m5-panel-check.sh          # every scenario card in turn against a running binary, about 13 minutes
```

Environment: `PORT` (8080), `SEED` (1), `KS_BASE_RATE` (300 events/s), `KS_DB_DIR` (the SQLite file's directory, default the OS temp dir), `KS_SYNC` (`normal`), `KS_WAL_AUTOCHECKPOINT` (4000).

## Deploy (Cloud Run, from a laptop with Docker and gcloud)

```
IMG="$REGION-docker.pkg.dev/$PROJECT/killswitch/killswitch:$(git rev-parse --short HEAD)"
docker build -t "$IMG" . && docker push "$IMG"
gcloud run deploy killswitch --image "$IMG" --region "$REGION" --platform managed --allow-unauthenticated \
  --min-instances 1 --max-instances 1 --cpu-boost --no-cpu-throttling --timeout 3600 --memory 1Gi --port 8080
```

One always-on instance (the D9 fallback in [docs/plan/ARCH-NOTES.md](docs/plan/ARCH-NOTES.md)): the World is a single process, pauses its traffic when nobody is watching and rebuilds after 10 s idle. The Artifact Registry repository is created once with `gcloud artifacts repositories create killswitch --repository-format docker --location $REGION`.

## What is where

`internal/cmek` is the core: key leases, the DEK cache and the state machine per tenant. `internal/kms` is the fake providers (real AES-256-GCM wrapping via Tink) with fault injection and a ground-truth log. `internal/queue` is the SQLite queue, scheduler and workers; `internal/admit` the fair-share admission; `internal/traffic` the load generator and the webhook sink; `internal/metrics` the snapshot the page streams; `internal/check` the checker that judges the invariants from the stored rows, the sink's record and the KMS ground truth; `internal/world` wires one complete system and its scenarios; `web/` is the page.

## Rationale

The written rationale is [docs/RATIONALE.md](docs/RATIONALE.md): the two promises, the key lease as the one knob, down versus revoked, isolation, the decision log D1–D9 and what more time would buy. The video (about five minutes, one take: revocation with a manual Restore, the 60 s outage, the tenant surge, then the core package on screen): _link set by Ben_.

## AI transcripts

Claude wrote the code, the tests and the drafts at Ben's direction; Ben reviewed every line of `internal/cmek` (1,094 lines across six files, 887 of them code) and made every decision, and the decision log in the rationale says who made each call. The spec conversations, the planning sessions and the build sessions are in [docs/transcripts/](docs/transcripts/README.md), raw and unedited, named by stage and date.

## Time spent

Build clock: _the Total row of [docs/TIMELOG.md](docs/TIMELOG.md), filled by Ben_. Spec and planning on claude.ai happened before the build clock and are not in the 4:45 budget.
