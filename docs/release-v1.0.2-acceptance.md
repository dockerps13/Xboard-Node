# Xboard-Node v1.0.2 final release acceptance

**Status: NOT APPROVED FOR STABLE.** All evidence must name the same immutable
source commit and matching package hashes. The candidate was prepared from
`604099881cd70a218826f693f24f0f22903e5876`; use the latest release
candidate branch head after CI completes, not that earlier SHA, as the eventual
final build input.

## Confirmed automated evidence

- [x] Hysteria2/TUIC optional TCP probe implementation and listener lifecycle
  tests added; parent code merge `6040998`.
- [x] Full GitHub Actions validation of pre-merge implementation and added
  lifecycle tests passed on `953c423` (run 44897748549).
- [ ] Full validation of the **final candidate commit** passes: formatting,
  Go test, race checks, vet, Linux amd64/arm64 builds, installer safety,
  Redis reservation tests and mixed protocol load.
- [ ] Final artifacts have an immutable SOURCE_COMMIT and verified SHA256SUMS.
- [ ] Installation, checksum validation and rollback tested using those exact
  candidate binaries.

## Actual machine/node acceptance (not satisfied by CI)

- [ ] On the AWS staging machine, disable the temporary TCP `socat` listeners
  on the probe ports, enable `kernel.tcp_probe: true` on the candidate,
  and confirm the *xboard-node binary itself* owns both TCP listeners.
- [ ] Shadowrocket shows TCP port-check latency for Hysteria2 and TUIC while
  both UDP/QUIC proxy sessions still fetch an actual webpage.
- [ ] Restart, reload, node deletion, port change and conflict scenarios
  release/reuse TCP sockets without affecting existing UDP listeners.
- [ ] Verify actual real XBoard panel traffic debit for both directions,
  duplicate ACK/lost reply and retry after crash, panel timeout, queue
  restart and midnight/rate boundary. Confirm no duplicate or missing
  charges; compare ledger and user balance.
- [ ] Verify online/offline reporting (including empty snapshot), stale
  device cleanup, user disable, plan exhaustion, IPv4/IPv6 normalization,
  fail/reconnect/reboot behavior, and at least two nodes concurrently.
- [ ] Explicitly approve a documented device policy: current distinct-IP
  observations are **not** strict global admission across servers. If strict
  global device caps are a release promise, finish and test the central
  reservation service and all participating nodes before declaring stable.
- [ ] One representative real-client case per supported protocol/transport,
  covering both kernel paths where supported; record exact client version
  and protocol/transport. Unsupported combinations are marked unsupported.
- [ ] TLS/ACME first issue, sharing, renewal and fail-closed behavior on
  multiple nodes, including restart and rate-limit failure.
- [ ] At least 24 hours of representative staging workload with CPU, RSS,
  goroutines, FDs, connections, error rate, restarts, user/device counts
  and actual traffic reconciliation.
- [ ] Installer upgrade/automatic rollback verified against existing
  configuration and credentials without deleting pending traffic journals.

## Release control

Do not claim the optional XBoard PHP integrations are deployed merely
because the repository contains them. `integrations/xboard-idempotent`
and `integrations/xboard-device-snapshots` remain opt-in candidates
requiring real panel schema, controller wiring, Redis and queue verification.

A green CI proves software checks only. Do not create a stable `v1.0.2`
tag or update a production `latest` marker until all applicable checkboxes
are evidenced and approved. If strict cross-node limits are excluded from
this release, mark that explicitly in the release notes and user-facing
feature scope; do not silently advertise the feature as supported.

After approval, the release should contain `xboard-node-linux-amd64`,
`xboard-node-linux-arm64`, `xbctl-linux-amd64`, `xbctl-linux-arm64`,
`install.sh`, `SOURCE_COMMIT`, `CHANGELOG`, and `SHA256SUMS`
built from one pinned tag.
