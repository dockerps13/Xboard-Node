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
- [x] Full CI validation of interim candidate `6ef1348ebc1648bccbdbaf8da2ba3ba761823099` passed (GitHub Actions run `44974751757`). This includes formatting, tests, race, vet, amd64/arm64 build, installer, Redis primitive and mixed protocol load.
- [ ] Re-run full validation on the **final frozen release commit**; the interim result does not approve a different commit.
- [ ] Final artifacts have an immutable SOURCE_COMMIT and verified SHA256SUMS.
- [ ] Installation, checksum validation and rollback tested using those exact
  candidate binaries.

## Real-world staging evidence (partial, user-observed; 2026-11-22)

- AWS staging machine `ip-172-31-42-230`, machine_id 24, ran `v1.0.2-rc-devicefix` (binary SHA256 `69e66a495aac8dbca1aea0f4484e0d91eb5775fb01a4bbbf87e571cf57eeae46`). Its banner is a manually set version and does not independently prove embedded git commit identity.
- `systemctl restart xboard-node` changed MainPID `723352` to `727632`; service was `active` after 10 seconds with no journal warnings for the preceding two minutes.
- User tested two simultaneously functioning devices with distinct public IPs; a third different-IP device could not use the proxy. After one of the first two disconnected, third became usable. The XBoard display stayed at `2/2`. A repeated test after restart again admitted the first two and rejected the third. This validates the **observed same-machine scenario**, not cross-VPS quota, all protocols, or long-lived disconnect behavior.
- Historical node log showed successful reporting from SOCKS5+TLS: `node_id=21 machine_id=24 device_report_users=1 device_report_ips=2` over repeated minute intervals. Xray-side traffic increments were confirmed in an earlier test.
- The fast WS event notification candidate `6ef1348` **was not on that AWS host** at the time of the above tests. Its end-to-end latency, offline delivery and actual browser auto-refresh remain unverified.
- P0 blocks remain: **cross-VPS atomic device reservation**, actual XBoard billing idempotency and offline/empty-state behavior, multi-node/mixed-kernel churn, 24h soak, exact final release binary hashes.

## Billing concurrency regression coverage

- [x] Added an isolated HTTP integration test for two simultaneous node-scoped journals sharing one simulated atomic panel ledger. Each node sends a report, the panel commits its charge but loses the response, and each client restarts with its original on-disk batch ID. The test asserts two separate node-scoped charges, zero duplicate debit and zero pending batches after the retry.
- [ ] Execute this test and the full offline acceptance suite on the final candidate SHA; a committed test is not yet a passing test.
- [ ] Verify the actual XBoard Laravel endpoint, transaction indexes, async queue job, DB deductions and Redis notifications with a backed-up test panel. A simulated idempotent ledger **does not prove** the production panel implements deduplication.

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

## Next verification order

1. Test pinned `6ef1348` or a newer CI-clean candidate in isolated AWS staging: WS connection/close changes, empty offline snapshot, panel browser without refresh, no billing behavior changes. Record timestamps and identify whether the panel consumes `report.devices`.
2. Implement the authenticated centralized Redis device lease API and node-side acquire/renew/release before claiming cross-VPS strict 2/2. The existing `integrations/xboard-device-snapshots` Lua primitive alone is insufficient.
3. Validate real XBoard report-ID deduplication, duplicate ACK/lost HTTP response, queue crash and rollback against a backed-up panel. Do not replay ambiguous legacy traffic as a new charge.
4. Repeat 24h soak, upgrade/rollback and artifact checksum validation against one frozen SHA, then review all remaining boxes.

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
