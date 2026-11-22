# Device admission P0: staged implementation

## Reproduction (2026-11-22)

In machine mode, the same account was configured with a limit of two distinct public IPs. Two clients connected successfully to SOCKS5+TLS, and a third different IP was rejected on that protocol, but was admitted via another protocol. The panel later showed **3/2**. This is a confirmed cross-protocol enforcement gap, not merely a delayed counter.

## Stage 1 (this PR): local atomic authority

`internal/devicegate.Registry` performs a single-lock check-and-reserve by (panel/machine scope, user ID, normalized client IP). A release callback is per connection and idempotent. Multiple protocols on one IP share one slot; different user/panel scopes are isolated; concurrent distinct IPs cannot exceed the cap. The new tests exercise saturation, release order, normalization, invalid inputs, and 100 concurrent admissions.

**Important: the new Registry is not wired into live kernels.** Merging this PR alone does not change actual node device enforcement. It is an independent, testable foundation. Do not mark the 3/2 reproduction fixed or publish a stable tag based on its tests.

## Stage 2: per-machine integration (required)

- Orchestrator owns a Registry per authenticated machine/panel and injects it into each service, including after node restart or kernel switching.
- Sing-box must reserve before accepting routed TCP/UDP traffic and release the exact reservation on every connection close and setup failure.
- Xray must reserve after the authenticated user and source IP are available, before passing traffic to outbound; release on connection teardown and every failure path.
- Use the panel's user ID consistently across both kernels; never use protocol login aliases to scope separate quotas.
- Add mixed-kernel tests: SOCKS5+TLS (Xray) + VLESS (sing-box), concurrent different IPs, same IP, node switching, forced close, reload, and disconnect.
- Do not reuse an unverified stale online-IP snapshot as an atomic reservation.

## Stage 3: cross-server strict mode (required)

In-memory Registry state cannot coordinate different machines. Implement an authenticated and authorized central lease API backed by the experimental Redis script in `integrations/xboard-device-snapshots/redis/device_reservation.lua`. All nodes must acquire **before** permitting a new device, renew while connected, and release at last disconnect. The authority must fence connection generations, expire abandoned owners, normalize IPv4/IPv6, and reject new devices when reservation is unavailable. Verify simultaneous admissions on different machines, Redis disruption and recovery, mid-connection lease expiration, machine restart, and migration of mixed old/new agents.

Do not confuse asynchronous online-device reporting (which can show 3/2) with admission control. Panel changes and data migrations must be tested on a backed-up staging copy before enabling live enforcement.

## Release gate

Status **P0 OPEN** until a third distinct IP is rejected on *every* protocol and *every* participating machine, and two simultaneous accepted IPs continue to work. Devices behind one public IP are counted as one because the current policy is IP-based, not a hardware fingerprint.
