# AutoKernel: Machine-mode commercial compatibility

> **Status:** development branch / draft PR. CI passing is a necessary
> gate, not proof of interoperability with a live XBoard installation.

## Goals

- Keep the CLI unchanged: `--mode machine --panel URL --token TOKEN --machine-id ID`.
- Fetch the machine's bound node list and choose a kernel **per node**.
- Preserve XBoard's existing users, limits, billing rate and traffic API.
- Fail loudly for unsupported protocol/transport/plugin combinations.
- Retain the last viable running node configuration after a bad WS update.
- Protect traffic counters across kernel swaps and report final deltas on shutdown.

## Protocol selection

| Protocol exposed by XBoard | Backend |
| --- | --- |
| Shadowsocks (plain) | sing-box or Xray |
| Shadowsocks + SIP003 HTTP simple-obfs | sing-box + native Go HTTP frontend (experimental) |
| VMess | sing-box or Xray, XHTTP → Xray |
| Trojan | sing-box or Xray, XHTTP → Xray |
| VLESS | sing-box or Xray, XHTTP / decryption → Xray |
| Hysteria / Hysteria2 | sing-box |
| TUIC | sing-box |
| AnyTLS | sing-box |
| SOCKS | sing-box or Xray |
| Naive | sing-box |
| HTTP proxy | sing-box or Xray |
| Mieru | sing-box |

### Shadowsocks HTTP obfs design

When the XBoard panel sends `plugin=obfs` and
`plugin_opts=obfs=http;obfs-host=...`, sing-box is selected automatically.

- Public TCP port: independent Go HTTP simple-obfs preface handler.
- Private TCP port: randomly selected loopback-only sing-box Shadowsocks inbound.
- Public UDP port: native sing-box Shadowsocks UDP inbound, unchanged.
- Both inbounds use the same users and traffic tracker.
- The native Go listener records original client IPs by loopback session source
  port to keep IP/device counters accurate. Upstream sing-box has removed
  `proxy_protocol`, so this implementation does not depend on it.
- The initial HTTP preface is bounded and deadline-limited.
- A rejected or unsupported HTTP request is closed.
- `obfs=tls` and non-simple-obfs plugins are **not yet implemented** and must
  return an explicit configuration error.

**Limitations:** client-to-server real-world interoperability (Shadowrocket,
v2rayN, other clients) and sustained load testing are still required. The
HTTP-fronted implementation must not be advertised as a hardened production
security control merely because configuration and loopback integration tests
pass.

## Billing compatibility

Node reports remain per-user **raw bytes**, indexed by XBoard user ID:

```json
{"traffic":{"123":[1024,2048]}}
```

XBoard applies its own current node rate including time-based rate windows
when it handles the report. The node backend must **never multiply rate**.

Tracker values are cumulative while the service is running; the reporting
layer sends increments. Hot-reload and user removal sample trailing counters.
Retired sing-box instances keep contributing bytes until drained; Xray retains
its cumulative baseline across swaps.

**Current shutdown behavior:** when the final report fails, the service
restores the pending bytes into its in-memory tracker, preserving them for a
controlled retry within that process. These bytes are **not durable across
process exit or machine restart**; this change does not solve lost reports.

### Unconfirmed report audit (implemented)

During a failed **final shutdown report**, the node writes raw per-user bytes
to `kernel.config_dir/unconfirmed-reports/<batch-id>.json` (mode 0600), using
an atomic rename plus fsync. Machine mode stores separate journals under each
node's own config directory. On the next startup the node warns about pending
records; malformed records fail closed.

**These records are for manual reconciliation, not retry.** A lost HTTP
response is ambiguous: XBoard may already have billed the batch. The node
must never replay these records automatically against the current
non-idempotent API. This audit feature does not preserve traffic that has
not yet been sampled, and does not journal periodic reports before sending.
The report journal package exposes an `Acknowledge` operation only for future
use with an explicit panel-side idempotent receipt.

### Opt-in idempotent wire contract (node implementation)

The new reporting path is **disabled by default**, including on existing
`soulook.org` installations. A compatible panel must respond to the V2
handshake with:

```json
{"settings":{"push_interval":60,"pull_interval":60,"idempotent_reports":true}}
```

Then each traffic report carries a stable `report_id` (32 lowercase hex
characters) and the normal raw `traffic` map. The panel must return a
**matching, explicit receipt**, for example:

```json
{"data":{"report_id":"0123456789abcdef0123456789abcdef","accepted":true}}
```

The node writes the batch to
`kernel.config_dir/idempotent-reports/<report_id>.json` **before sending**,
retries the *same* ID after errors/restarts, and removes the local batch
only after validating the matching receipt. Each reporting tick drains at
most four pending batches, avoiding unbounded blocking under high load.

Do not advertise `idempotent_reports=true` on the panel until the panel has
the transactional database ledger and worker described below. A legacy
HTTP 200 response without a matching receipt leaves the batch on disk.
An `accepted=true` receipt means durable acceptance, not necessarily that
asynchronous billing has completed.

### Panel implementation required for commercial billing

Before enabling automatic durable retries or labelling the deployment
"exactly-once", implement an atomic report ledger in the XBoard panel:

1. The node generates one unchanging report ID per raw-traffic batch.
2. The panel durably records `(node_type, node_id, report_id)` with a
   unique index, raw bytes, and the chosen time-dependent rate at receipt.
3. A queued worker atomically applies user, user-stat, and server-stat
   increments together with marking that batch as applied. Re-running the
   worker must not increment twice.
4. An identical report ID with a different payload must be rejected.
5. The API acknowledges a batch only after it is stored durably, and
   supports idempotent replay of the exact same report.
6. Existing legacy nodes retain their original reporting endpoint and
   behavior during migration.

Do not switch to this new protocol merely because a node-side journal exists.

**Outstanding mandatory commercial gate:** exactly-once billing cannot be
guaranteed by a node-only change. The existing panel accepts non-idempotent
traffic updates. It needs a database-backed report ID + atomic deduplication /
queue ledger. Node-side retries alone can duplicate billing after a lost HTTP
acknowledgment. Do not claim this risk is solved yet.

## Release gates

1. `go test ./...`, `go vet ./...`, relevant `go test -race`, and
   linux/amd64 build must pass.
2. Validate every protocol with actual client authentication, traffic,
   rate limits, online IP accounting, and user removal.
3. Validate HTTP simple-obfs against at least two real client
   implementations, plus UDP and high-concurrency scenarios.
4. Validate reporting under HTTP timeouts, lost responses, kernel restarts,
   panel restart, and XBoard time-dependent rate changes.
5. Run canary on an isolated VPS and XBoard machine ID, then verify rollback.
6. Only after that, merge and publish fork-owned release artifacts. Never
   overwrite the currently working official deployment as an untested step.

## Installer and secrets

The installer in this fork references fork-owned GitHub Releases rather than
the upstream vendor's release, preventing silent installation of an unmodified
binary. Do not commit or paste machine tokens. Rotate exposed tokens.
