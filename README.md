# xboard-node

Node backend for [Xboard](https://github.com/cedar2025/Xboard). Supports `sing-box` / `xray-core` dual kernels.

> **Disclaimer**: This project is for educational and learning purposes only.

## Features

- Protocols: VLESS/VMess, Trojan, Shadowsocks, Hysteria2, TUIC, SOCKS5,
  HTTP/HTTPS, AnyTLS, Mieru, and Naive (kernel/client support varies)
- Sync: WebSocket push + REST polling dual channel
- User controls: speed limit, device limit, alive-IP tracking, hot update
- Deploy modes: node mode, machine mode, standalone mode
- Multi-instance: single process binding multiple panels / nodes

## Install

This fork's Linux systemd releases are published at [dockerps13/Xboard-Node Releases](https://github.com/dockerps13/Xboard-Node/releases). See the [deployment and operations guide](docs/deployment.md) for pinned version installation, upgrade, backup, rollback and troubleshooting.

**Current release status:** V1.0.1 is in final release preparation and has not been published. V1.0.0 remains the production baseline. Do not install from a moving branch, use `latest`, or treat the upstream Docker image as a validated V1.0.1 build. After approval, use the exact V1.0.1 tag together with its `SOURCE_COMMIT` and `SHA256SUMS`.

Docker examples from the upstream project are not release-validated deployment instructions for this fork.

## xbctl

Run `xbctl` after installation for help. Common commands:

```bash
xbctl list                          # list all instances
xbctl status                        # running status
xbctl bind add-node --panel URL --token TOKEN --node-id 1
xbctl bind add-machine --panel URL --token TOKEN --machine-id 1
xbctl bind remove-node --panel URL --node-id 1
xbctl service restart
```

## Configuration

Legacy single-panel config is fully compatible. Appending bindings auto-migrates to `instances` format. See `config.yml.example`.

## Extensions

- Custom routes: [docs-custom-routes.md](docs-custom-routes.md)
- Custom outbounds: [docs-custom-outbounds.md](docs-custom-outbounds.md)
- DNS providers (ACME DNS-01): [docs-dns-providers.md](docs-dns-providers.md)

## License

MPL-2.0.
