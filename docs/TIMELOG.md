# Time log

Working minutes per lane, per milestone (docs/plan/README.md › Working asynchronously). Planning time before the build clock is not counted (HANDOFF). [C] = Claude in the session, [B] = Ben on his laptop.

| Milestone | Planned | Actual [C] | Actual [B] | Started | Ended | Notes (cuts, fallbacks, decisions, parameter changes) |
| --- | --- | --- | --- | --- | --- | --- |
| M0 Skeleton and deploy | 20 | | | 7:39pm | 7:59pm | Live on Cloud Run; ticker check A PASS (2026-09-22: ticks_advanced=122, max gap 0.61 s): CPU flows while a stream is open; B (idle throttling) could not be measured: the viewer count never returned to 0 on Cloud Run after all clients closed (ARCH-NOTES 11); D9 fallback taken: --min-instances 1 --no-cpu-throttling; /health replaces /healthz (Cloud Run reserves /healthz); all three M0 decisions as recommended |
| M1 Data path | 45 | 29 | | 8:58pm | 9:27pm | Claude wrote everything (Ben reviews the four tests at his pickup); commits af38c48 `M1: data path`, 3e4a6b5 (generator fix). Measurement on /dev/shm, 60 s at 1,500/s: insert_mean_us 64 (rule ≤ 300 → no fallback, level 0: synchronous=NORMAL, wal_autocheckpoint 4000), insert_max_us 19,769 (WAL checkpoints; informational), ticks 121, delivered 609/s (capacity 640), internal 0, by_state [1000,0,0,0], backlog 50,607 rows (86 MB); S1 `grep -ac PLAINTEXT-CANARY` on the .db and -wal: 0 and 0. 300/s for 20 s (fresh process): accepted = delivered = 294/s, backlog 0, one generate per tenant (kms ok front-loaded). curl: 202 `{"id":87588,"tenant":"t-0042"}`, 404 unknown_tenant, 400 without X-Tenant-ID. Deviations: generator moved to an absolute arrival schedule (sleep-then-send gave 1,455/s at 1,500 offered; 1,508/s after, 20 s run); `Reclaim` built (not deferred); tenant-detail endpoint built minimally (not cut; M4 fills the lease fields); `Store.Dead` decrements total/backlogged; `metrics.OfferedSource` added; import-check regex gains the dot (go1.26 `entropy/v1.0.0`); `.gitignore` for the binary. `TestTwoWorlds` green under -race. `BenchmarkInsert` ns/op on Ben's laptop: pending |
| M2 CMEK core | 45 | | | | | |
| M3 Admission and surges | 30 | | | | | |
| M4 UI and scenarios | 55 | | | | | |
| M5 Checker and hardening | 25 | | | | | |
| M6 Rationale | 45 | | | | | |
| Buffer | 20 | | | | | |
| **Total** | **285 (4:45)** | | | | | |
