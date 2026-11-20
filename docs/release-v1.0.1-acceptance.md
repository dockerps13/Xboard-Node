# V1.0.1 release acceptance

V1.0.1 is pending approval. V1.0.0 remains the production baseline until every
required item below has evidence for one exact source commit. No RC, Beta, or
stable Release is created by this checklist.

## Automated evidence

- [ ] GitHub Actions formatting, full Go tests, vet, installer safety, and panel patch checks pass.
- [ ] `go test -race` passes for control-plane, certificate, tracker, billing, kernel, and AnyTLS fork packages.
- [ ] Linux amd64 and arm64 `xboard-node` and `xbctl` binaries build and report `v1.0.1`.
- [ ] Release artifacts contain only the matching binaries, `install.sh`, `SOURCE_COMMIT`, `CHANGELOG`, and `SHA256SUMS`; every listed hash verifies.
- [ ] Ordinary multi-node ACME tests prove one issuer call, shared certificate reads, renewal, restart recovery, lock serialization, shared-directory failure handling, and migration recovery.
- [ ] The Xboard compatibility patch applies cleanly to upstream commit `d826a3b32f73bde24acce2a3ab033c6538ce121d`, passes PHP lint, and rejects unsupported subscription protocols with HTTP 422.

## Required real-environment evidence

- [ ] Two ordinary nodes using one domain start concurrently against the same panel and load the same certificate without a duplicate issuance.
- [ ] Certificate renewal is observed by both listeners; failed ACME never starts a plaintext listener and recovery restores the TLS listener.
- [ ] AnyTLS real clients pass normal, wrong-password, and deleted-user tests; user hot update, deletion, traffic, and online-device reports are checked.
- [ ] Naive HTTP/2 real data transfer is verified with Cronet/Shadowrocket, not only a TLS handshake; authentication and subscription fields are checked.
- [ ] HTTP CONNECT and ordinary HTTP forwarding are tested with multiple clients, including the reported Shadowrocket/Telegram/Google case, separating client behavior from server behavior.
- [ ] Mieru TCP/UDP, traffic pattern, advanced fields, hot update, and subscription parsing are tested with a Mieru client.
- [ ] The fifteen-protocol matrix is run end to end, with unsupported client/protocol combinations marked unsupported rather than inferred successful.
- [ ] Online devices, explicit empty offline reports, node switching, report failure recovery, IPv4/IPv6 normalization, deletion cleanup, restart recovery, and billing idempotency are verified against the real panel.
- [ ] At least 24 hours of representative staging workload records RSS, CPU, goroutines, descriptors, connections, restarts, errors, traffic totals, and device counts.

## Security and rollback

- [ ] `third_party/sing-anytls/LICENSE` (GPLv3-or-later) and its pinned source provenance ship with the source release.
- [ ] TLS certificate and private-key validation, private-key permissions, fail-closed behavior, and no insecure default are reviewed.
- [ ] The installer backup manifest verifies before upgrade and automatic rollback restores the previous binary, CLI, config, credentials, metadata, and service unit.
- [ ] The rollback path to the verified V1.0.0 installation is rehearsed without deleting runtime journals or pending billing records.

Do not publish V1.0.1 while any real-environment item is unverified. Record the
client, version, node type, kernel, exact source commit, and sanitized evidence
for every real-client result.
