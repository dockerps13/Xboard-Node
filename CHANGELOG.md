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
- Preserve the existing protocol, limiter, traffic journal, installer and
  kernel regression suites from the development baseline.

Pending release blockers: on-host acceptance of the built-in TCP probe after
removing the temporary `socat` listeners; real panel billing and offline
device/online count recovery; explicit scope decision about strict
cross-node admission; upgrade/rollback and representative soak evidence.

## V1.0.1 (pending release)

This entry describes the source currently under final validation. It is not a
published Release and must not be installed in production until the release
gate is approved.

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
