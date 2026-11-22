# Changelog

## V1.0.2 (release candidate — NOT STABLE)

This version remains under final release acceptance. No stable tag or release
may be published until the release gates in
[docs/release-v1.0.2-acceptance.md](docs/release-v1.0.2-acceptance.md) pass for
one pinned commit.

- Add opt-in `kernel.tcp_probe: true` for Hysteria2/TUIC UDP nodes. It
  offers a bare TCP handshake on the corresponding port to support clients
  using direct TCP port checks. It is off by default, is not QUIC RTT, and
  does not change UDP proxy handling.
- Add lifecycle and port conflict regression tests for the TCP listener.
- Add event-triggered device snapshot updates for Xray and Sing-box over the existing WebSocket transport, with 350ms coalescing. Normal traffic billing and its report interval remain unchanged. Panel-side event handling/browser refresh must be independently verified.
- Bind Laravel idempotent billing accept/recovery/job dispatches to an isolated Redis queue connection `redis_idempotent` with staging-only 240-second retry_after and 210-second Horizon timeout guidance; legacy production Redis retry_after=90 remains untouched. Added source contract tests for queue wiring.
- Route optional Laravel idempotent ledger jobs to a dedicated `idempotent_traffic` queue; production `traffic_fetch` Horizon workers have a 30-second timeout, incompatible with a 180-second atomic ledger job. Deployment remains disabled pending dedicated-worker and database acceptance.
- Add overflow-checked raw and rate-adjusted billing counters for the optional Laravel idempotent integration, with isolated PHP boundary tests. This integration is not deployed or enabled on the live panel.
- Add concurrent multi-node HTTP billing replay regression: separate journals with lost ACKs replay the original report IDs and an idempotent test ledger charges each node exactly once. Actual XBoard panel acceptance is still required.
- Fix Xray SOCKS5+TLS and HTTP user traffic accounting: Xray binds user statistics to the authenticated login (UUID), rather than the node's synthetic user@ID key. Add counter identity regression tests. Historical unreported traffic is not automatically back-billed.
- Preserve the existing protocol, limiter, traffic journal, installer and
  kernel regression suites from the development baseline.

Pending release blockers: on-host acceptance of the built-in TCP probe after
removing the temporary `socat` listeners; real panel billing and offline
device/online count recovery; explicit scope decision about strict
cross-node admission; upgrade/rollback and representative soak evidence.

## V1.0.1 (published 2026-11-20)

GitHub Release v1.0.1 was published on 2026-11-20. The notes below describe
its development work; v1.0.2 remains a separate unreleased candidate.

- Share ACME account and certificate storage across ordinary `nodes:` instances,
  with migration, file locking, retry/backoff, renewal reload, validation, and
  fail-closed recovery coverage.
- Preserve AnyTLS authentication and hot-update behavior, including the local
  race-safe fork and wrong-password/deleted-user regression coverage.
- Keep HTTP/HTTPS, Naive, SOCKS-over-TLS, and Mieru protocol fields explicit;
  unsupported subscription combinations are rejected instead of silently
  converted or dropped.
- Add the complete Xboard compatibility patch for upstream commit
  `d826a3b32f73bde24acce2a3ab033c6538ce121d`, including node-config
  normalization, UUID credential validation, subscription protocol assertions,
  HTTP 422 reporting, and PHP snapshot tests.
- Keep amd64/arm64 build metadata, checksums, installer backups, and rollback
  inputs tied to one exact source commit.

Known release gate: real server and client interoperability evidence (including
Cronet Naive, Shadowrocket, AnyTLS, Mieru, HTTP/HTTPS, and the full fifteen-item
matrix) must be recorded before publication. No production panel or server is
changed by this source tree.

## V1.0.0

Production baseline. See the historical [V1.0.0 acceptance record](docs/release-v1.0.0-acceptance.md).
