# Killswitch

A CMEK-compliant multi-tenant webhook event queue. Each tenant owns the key that protects its data: revoke it and the provider can no longer read anything of that tenant's, within a published bound of 30 seconds. One tenant's KMS trouble, revocation or traffic surge changes nothing for the other 999, because every message is sealed under a per-tenant data key that is leased and cached, so the KMS is off the request path and a KMS blip is ridden through instead of felt. The invariants that say so are judged live, from outside the service, on the page.

Live URL: _set by Ben after the M5 deploy_. Spec: [docs/SPEC.md](docs/SPEC.md). Plans and decisions: [docs/plan/](docs/plan/README.md). Time log: [docs/TIMELOG.md](docs/TIMELOG.md).

## What to click

The page is one shared live system: a thousand tenants spread across three simulated key providers (aws, gcp, azure), a grid with one cell per tenant, a few tiles and charts, the invariant panel and a timeline. Each card injects a fault or a surge and says what to watch; every card has a Stop, and Reset world rebuilds everything under a new id. Traffic runs only while a tab is open.

- **Slow KMS.** One provider, azure, becomes slow enough that many of its calls run past the timeout. What happens to the rest of the service depends entirely on where the key fetch runs, and the three key-fetch buttons (in the header and on the card) switch that design live for the whole service. *Naive*: no lease, no cache, no bulkheads, every event and delivery makes its own key call and waits; the slow provider's calls hold the shared delivery workers, so delivery collapses for every tenant and the key-call rate explodes. *Sync*: the cache and the lease are back, so calls are rare and capped, but the worker that finds a lease due still fetches inline, and every tenant's deliveries queue behind those waits: throughput mostly survives, latency does not. *Async*, the design that ships: a lease due for renewal is refreshed in the background while deliveries keep using the cached key, so azure's slowness stays azure's problem and the backlog drains the moment you switch to it. A new World starts in Async.
- **Provider outage.** One provider, gcp, stops answering. Its tenants are still holding cached keys, so nothing has to stop right away. *Blip*: gone for a few seconds; the affected cells turn yellow while renewals fail and the service rides through on the leases it has, then everything is green again with nothing delayed or lost. *Outage*: gone for a minute; yellow spreads with no customer impact, and only once a tenant's lease runs out does its cell turn orange, its new events turned away with a clear error and its queued messages waiting, sealed. Retries back off instead of hammering the dead provider, the other providers' tenants never notice, and the waiting messages deliver when gcp returns.
- **Key revocation.** One busy tenant revokes its key, the promise the system exists for: from that moment the provider can no longer read anything of that tenant's, and within a fixed bound it stops trying. The cell turns purple as soon as the service notices, new events are refused with an error that says why, the keys held in memory are purged, and the queued messages stay sealed. The timeline shows the revocation as the key provider saw it and how long detection took; the checker's light on it reads ground truth, not the service's own word. Restore gives the key back and the waiting messages deliver.
- **Tenant surge.** One busy tenant sends a hundred times its usual traffic. Its cell stays green with a ring; above the rate it is allowed, its extra events are turned away with a rate-limit error, so it cannot crowd out anyone else. Its key traffic does not grow with its event traffic, which is what the cache and the lease buy. The other tenants' latency does not move.
- **Global surge.** Every tenant sends several times its usual traffic at once, more than the service can deliver. Delivery holds at full capacity, the queue absorbs the rest, the overflow is rejected at the door heaviest senders first, and tenants within their fair share keep getting through untouched. The backlog levels off under its cap and drains once the surge ends; key traffic stays flat, because a surge of events is not a surge of key fetches. L1 is not judged here, since every tenant is offered load; L4 is.

The no-key-cache runs (`no_cache`, `no_cache_surge`: the cache removed with the bulkheads kept and a realistic latency on every provider) have no card but remain as scenarios for the API and the script. Cards can be hidden one at a time (the × in each corner) or as a column (Hide cards); a hidden card's scenario keeps running and the timeline still reports it. The panel's six lights are judged once a second by a checker that reads only the stored rows, the sink's delivery record and the fake KMS's ground truth, never the service's own counters. One detail worth knowing when watching a revocation: a KMS reply that was sent before the key was disabled can arrive after it, so a late OK is ignored whenever it predates the deny and a revoked tenant can never be un-parked by a stale answer.

The measured numbers behind every card (rates, latencies, backlog sizes, detection times) are in the plans under [docs/plan/](docs/plan/README.md) and the time log.

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
