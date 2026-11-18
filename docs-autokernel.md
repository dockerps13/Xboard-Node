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
