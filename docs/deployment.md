# Linux deployment and operations

## V1.0.2 candidate deployment (not a production release)

V1.0.2 is an **unreleased candidate**. Do not use the `v1.0.2` tag,
`latest`, or a moving branch to install until the final gate is signed off
and published. See [V1.0.2 acceptance](release-v1.0.2-acceptance.md).

The opt-in `kernel.tcp_probe: true` setting adds a bare TCP listener at a
Hysteria2/TUIC node's UDP port for Shadowrocket-style direct TCP latency
checks. It reports TCP reachability, **not** QUIC RTT. Before testing the
built-in implementation, stop temporary `socat` listeners on those TCP
ports; otherwise the new listener will report a bind collision and skip
that endpoint. Keep the UDP service unaffected and test one node at a
time. Do not enable this on unrelated protocols. The option defaults to
off. Update the AWS security group as needed for the desired TCP tests,
and avoid exposing unnecessary listeners.

Candidate testing must preserve a verified copy of the prior executable,
`xbctl`, configuration, credentials, and systemd unit. Verify all hashes
and the exact `SOURCE_COMMIT`; never use a `latest` URL for staging
upgrades. Stage, observe, and rehearse rollback first. Real panel billing,
cross-node limits, node disable/re-enable, reporting and disconnect tests
are mandatory for a commercial release.

## Candidate: fast online-device reports

Xray and Sing-box notify the service when authenticated connections are added or closed. A coalesced 350ms event refreshes current per-user IP snapshots and sends the existing `report.devices` event when the panel WebSocket is connected; the periodic snapshot and regular REST/billing report continue unchanged. This is not a 350ms browser refresh guarantee: the panel WebSocket handler must accept `report.devices` and the XBoard user list must refresh its own view. When WebSocket is unavailable, the periodic REST path remains the durable fallback; device and traffic reports are not re-sent for every connection. Rejected device connections do not generate an online-device event. Test the real panel before promising 1–3s UI updates.

### Live-device acceptance

On staging: connect and disconnect two distinct public-IP clients and verify WebSocket `report.devices` snapshots include online and explicit offline records without touching user traffic accounting. Confirm the XBoard user list changes without manual refresh; if it does not, inspect panel-side event routing and browser polling separately. Test 1000+ connection churn, WS disconnected/reconnected, machine-mode shared WS, restart, and `2/2` rejection of a third IP. Never infer UI behavior from node logs alone.

## Historical v1.0.1 deployment guidance

The following v1.0.1 instructions are retained as historical candidate guidance, **not** as the v1.0.2 release procedure.

## Release status and scope

V1.0.1 is pending final approval and has no published stable assets yet. V1.0.0 remains the production baseline. Linux amd64 and arm64 release builds, `SOURCE_COMMIT`, `CHANGELOG`, and `SHA256SUMS` must all come from the same approved V1.0.1 tag. Do not use a moving branch or `latest` for the production upgrade.

Use a root shell on a systemd host with curl, CA certificates, sha256sum and sufficient disk space. The newer installer also requires flock (util-linux). Keep SSH access independent of the proxy being upgraded. Arrange a maintenance window: upgrades restart the service and briefly interrupt connections.

## Pinned V1.0.1 installation (after approval)

Download the installer from the matching immutable commit, review it, then run it. The release manifest detects corrupted downloads; it is not an independent signature of publisher identity.

```bash
curl -fL --connect-timeout 10 --max-time 60 \
  https://raw.githubusercontent.com/dockerps13/Xboard-Node/v1.0.1/install.sh \
  -o /root/xboard-install-v101.sh
bash -n /root/xboard-install-v101.sh
less /root/xboard-install-v101.sh
```

For a new machine-mode deployment (replace the URL and machine ID):

```bash
read -r -s -p 'Panel token: ' TOKEN; echo
bash /root/xboard-install-v101.sh install --version v1.0.1 \
  --mode machine --panel https://panel.example.com --machine-id 24 --token "$TOKEN"
unset TOKEN
```

For node mode use `--mode node --node-id ID` instead of the machine arguments. Do not paste real credentials into support tickets. Reading a token interactively avoids literal shell history, but the installer and xbctl still pass it in process arguments; privileged local observers may see it. Use a trusted host. Do not enable shell tracing.

The V1.0.1 installer requires an exact release version for production. Verify the downloaded `SHA256SUMS` before installation and keep the matching `SOURCE_COMMIT`. If V1.0.1 has not yet been published, stop here; use the V1.0.0 installation for production and a separately built test environment for validation.

## Upgrade and rollback

Before upgrade, record the installed version, service state, and secure copies of both executables, configuration, credentials, service unit and drop-ins. Keep application runtime state and reporting journals for investigation; do not blindly restore old journal files or erase pending traffic records. Verify backup checksums. Keep backups root-only.

```bash
/usr/local/bin/xboard-node -v
systemctl show xboard-node -p MainPID -p ActiveState -p NRestarts
systemctl cat xboard-node
```

The supported upgrade action preserves existing configuration and service unit rather than regenerating bindings:

```bash
bash /root/xboard-install-v101.sh upgrade --version v1.0.1
```

Inspect the reported backup path. If health checks or actual client tests fail, stop further rollout and use the verified V1.0.0 binaries and compatible configuration. The installer verifies the backup manifest and automatically restores the previous binary, CLI, configuration, credentials, install metadata, and service unit when an upgrade fails after mutation. If configuration or unit files were changed independently, restore the corresponding verified backups during the maintenance window. Never use `uninstall --purge` for rollback.

## Historical candidate records

Older engineering records refer to RC.5 and RC.6. Those names describe historical pre-release validation only; they are not supported installation targets and must not be relabeled as V1.0.1.

## Post-upgrade checks

```bash
/usr/local/bin/xboard-node -v
systemctl show xboard-node -p MainPID -p ActiveState -p NRestarts
ss -lntup | grep xboard-node
journalctl -u xboard-node -n 80 --no-pager
```

Check real client traffic, online refresh, offline reporting, panel count and billing. Health endpoint success alone is not proof all nodes work. Redact IPs, tokens, URLs containing credentials and user identifiers before sharing logs. For long-running monitoring record PID, restarts, RSS, CPU time, threads, descriptors and connections over representative load and subsequent idle periods. Baseline after controlled restart; distinguish intentional crash tests from unexpected failures.

## Device-count semantics and limitations

A device in the current implementation is a distinct observed source IP, not a physical phone or computer. NAT can merge multiple devices; mobile network transitions can temporarily count a single device twice. Successful device reporting is not proof of global atomic admission. REST global snapshots require compatible panel integration; Redis atomic reservations are experimental and are not wired into production admission. Do not expose or enable these unfinished integrations automatically. Legacy ambiguous billing reports require manual reconciliation.

See [V1.0.1 release acceptance](release-v1.0.1-acceptance.md) for the remaining gates. Do not publish stable/latest until the exact final candidate passes acceptance and receives explicit approval.
