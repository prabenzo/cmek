# Time log

Working minutes per lane, per milestone (docs/plan/README.md › Working asynchronously). Planning time before the build clock is not counted (HANDOFF). [C] = Claude in the session, [B] = Ben on his laptop.

| Milestone | Planned | Actual [C] | Actual [B] | Started | Ended | Notes (cuts, fallbacks, decisions, parameter changes) |
| --- | --- | --- | --- | --- | --- | --- |
| M0 Skeleton and deploy | 20 | | | 7:39pm | 7:59pm | Live on Cloud Run; ticker check A PASS (2026-09-22: ticks_advanced=122, max gap 0.61 s): CPU flows while a stream is open; B (idle throttling) could not be measured: the viewer count never returned to 0 on Cloud Run after all clients closed (ARCH-NOTES 11); D9 fallback taken: --min-instances 1 --no-cpu-throttling; /health replaces /healthz (Cloud Run reserves /healthz); all three M0 decisions as recommended |
| M1 Data path | 45 | | | | | |
| M2 CMEK core | 45 | | | | | |
| M3 Admission and surges | 30 | | | | | |
| M4 UI and scenarios | 55 | | | | | |
| M5 Checker and hardening | 25 | | | | | |
| M6 Rationale | 45 | | | | | |
| Buffer | 20 | | | | | |
| **Total** | **285 (4:45)** | | | | | |
