# v1.0.0 release acceptance

RC.5 is a feature-freeze candidate, not a stable release. Existing RC.4 assets remain unchanged. Do not promote by simply renaming a binary.

## Required gates (record evidence against exact candidate commit)

- [ ] Full CI, both architectures, checksum and version checks, Xray race tests.
- [ ] Isolated panel/node upgrade from RC.4, configuration unchanged, rollback to RC.4 verified.
- [ ] Real SS and VLESS connection, traffic accounting, online refresh, explicit offline and reconnect.
- [ ] Graceful restart and forced process termination recovery; Redis and DB device counts agree after successful report.
- [ ] Panel/API interruption and restoration; no duplicate billing, ambiguous legacy traffic audits reconciled.
- [ ] Same-IP and distinct-IP cross-node tests; record REST-only limitations. Do not claim strict distributed limits unless authenticated panel API and both kernel lease lifecycle implementations are deployed and tested.
- [ ] At least 24 hours real staging workload; record RSS, CPU, CPU time, goroutines, FD count, connections, restarts and errors; measure idle recovery after load. Investigate persistent growth or unexpected restarts.
- [ ] Installer explicit version and rollback documentation, release notes and known limitations reviewed.

Experimental PHP snapshot and Redis lease components are not installed automatically. Keep capabilities disabled until real panel authorization, Redis failure, and compatibility tests pass. Idempotent traffic integration remains opt-in. Stable release must be separately authorized after evidence review. No automatic stable/latest publication in this workflow.
