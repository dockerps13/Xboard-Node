# Changelog

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
