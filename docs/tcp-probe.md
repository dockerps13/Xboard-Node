# Optional TCP latency probe for Hysteria2 and TUIC

Hysteria2 and TUIC use UDP/QUIC. Some client TCP port-check latency tests send
TCP SYN packets to the configured endpoint, receive RST from a UDP-only port,
and show a blank result despite successful QUIC proxy traffic.

To opt in:

```yaml
kernel:
  type: singbox
  tcp_probe: true
```

This setting is **off by default**. On a successfully started Hysteria2 or
TUIC inbound, the process additionally listens for TCP handshakes on the
same numbered port, then immediately closes accepted sockets. UDP is unchanged.
Machine mode uses each discovered node's port. The listener is reused on a
same-port kernel replacement and closed when the node stops, changes protocol,
or moves to a different port. A TCP bind conflict is logged and does not
prevent the UDP proxy from starting.

**Important:** The reported latency is a TCP handshake/port reachability
measurement. It is *not* QUIC round-trip time, authentication success, or real
proxy-request latency. Prefer a real proxied HTTP connectivity test where the
client offers it. Enable only on trusted deployments with appropriate TCP
security-group/firewall policy; this opens extra TCP ports. Do not enable
alongside `socat` or another TCP service on the same port. For multi-instance
configuration, top-level `kernel.tcp_probe: true` is inherited by instances.

Run `go test ./internal/tcpprobe ./internal/kernel/singbox ./internal/config`
and verify an actual Shadowrocket client before enabling on production.
