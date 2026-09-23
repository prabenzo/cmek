# Killswitch

A CMEK-compliant multi-tenant webhook event queue. Each tenant owns the key that protects its data: revoke it and the provider can no longer read anything of that tenant's, within a published bound of 30 seconds. One tenant's KMS trouble, revocation or traffic surge changes nothing for the other 999, because every message is sealed under a per-tenant data key that is leased and cached, so the KMS is off the request path and a KMS blip is ridden through instead of felt. The invariants that say so are judged live, from outside the service, on the page.

Live URL: _set by Ben after the M5 deploy_. Spec: [docs/SPEC.md](docs/SPEC.md). Plans and decisions: [docs/plan/](docs/plan/README.md). Time log: [docs/TIMELOG.md](docs/TIMELOG.md).

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
