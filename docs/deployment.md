# Linux deployment and operations

## Release status and scope

The published RC.5 candidate is commit `0ad7092a19e07fc3079abd846bca32dace694ad7`. The newer development branch is not yet a released build. Do not represent the candidate as stable. Linux amd64 and arm64 binaries are built in CI; actual staging evidence currently covers amd64, REST machine mode, SS and VLESS. Other combinations require deployment-specific validation.

Use a root shell on a systemd host with curl, CA certificates, sha256sum and sufficient disk space. The newer installer also requires flock (util-linux). Keep SSH access independent of the proxy being upgraded. Arrange a maintenance window: upgrades restart the service and briefly interrupt connections.

## Pinned RC.5 installation

Download the installer from the matching immutable commit, review it, then run it. The release manifest detects corrupted downloads; it is not an independent signature of publisher identity.

```bash
curl -fL --connect-timeout 10 --max-time 60 \
  https://raw.githubusercontent.com/dockerps13/Xboard-Node/0ad7092a19e07fc3079abd846bca32dace694ad7/install.sh \
  -o /root/xboard-install-rc5.sh
bash -n /root/xboard-install-rc5.sh
less /root/xboard-install-rc5.sh
```

For a new machine-mode deployment (replace the URL and machine ID):

```bash
read -r -s -p 'Panel token: ' TOKEN; echo
bash /root/xboard-install-rc5.sh install --version v1.0.0-rc.5 \
  --mode machine --panel https://panel.example.com --machine-id 24 --token "$TOKEN"
unset TOKEN
```

For node mode use `--mode node --node-id ID` instead of the machine arguments. Do not paste real credentials into support tickets. Reading a token interactively avoids literal shell history, but the installer and xbctl still pass it in process arguments; privileged local observers may see it. Use a trusted host. Do not enable shell tracing.

The RC.5 installer can auto-detect local binaries in its working directory. Run it from a clean directory without unrelated xboard-node or xbctl files. Newer unreleased installers require explicit local binary options.

## Upgrade and rollback

Before upgrade, record the installed version, service state, and secure copies of both executables, configuration, credentials, service unit and drop-ins. Keep application runtime state and reporting journals for investigation; do not blindly restore old journal files or erase pending traffic records. Verify backup checksums. Keep backups root-only.

```bash
/usr/local/bin/xboard-node -v
systemctl show xboard-node -p MainPID -p ActiveState -p NRestarts
systemctl cat xboard-node
```

The supported upgrade action preserves existing configuration and service unit rather than regenerating bindings:

```bash
bash /root/xboard-install-rc5.sh upgrade --version v1.0.0-rc.5
```

This example is for upgrading an older release to RC.5, not reinstalling an already running candidate. Inspect the reported backup path. If health checks or actual client tests fail, stop further rollout and use the verified previous release and its compatible configuration. For a prior standard installation, a pinned upgrade to the verified previous version can restore binaries; confirm schema compatibility before doing so. If configuration or unit files were changed, restore the corresponding verified backups during the maintenance window. Never use uninstall --purge for rollback.

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

See [release acceptance](release-v1.0.0-acceptance.md) for remaining gates. Do not publish stable/latest until the exact final candidate passes acceptance and receives explicit approval.
