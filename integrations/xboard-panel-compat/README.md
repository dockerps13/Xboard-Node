# Xboard panel compatibility handoff for Xboard-Node V1.0.1

This directory is an independent handoff for the panel integration. It does
not modify `/www/wwwroot/soulook.org` and is not submitted to the Xboard
upstream repository.

The read-only comparison used `cedar2025/Xboard` commit
`d826a3b32f73bde24acce2a3ab033c6538ce121d`. Its current server form and
subscription builders already expose AnyTLS, Naive, Mieru, SOCKS TLS, HTTP,
and the AnyTLS `padding_scheme` field. The node-side adapter therefore keeps
the panel's existing wire names:

| Panel field | Node field | Required behavior |
| --- | --- | --- |
| `protocol_settings.padding_scheme` | `padding_scheme` | Preserve an array; the node accepts string or array form. |
| `protocol_settings.transport` | `transport` | Mieru only; preserve `TCP`/`UDP`, do not map it to generic multiplex. |
| `protocol_settings.traffic_pattern` | `traffic_pattern` | Mieru only; pass through unchanged. |
| `protocol_settings.tls` | `tls` / `tls_settings` | HTTP, Naive, and SOCKS TLS must retain TLS enabled state. |
| `protocol_settings.tls.allow_insecure` | client subscription only | Default must remain false; never use it to disable server certificate validation. |
| `protocol_settings.tls.server_name` | `tls_settings.server_name` | Preserve SNI independently of the node listen address. |
| `cert_config` | `cert_config` | Preserve `mode`/`cert_mode` compatibility and fail closed when material is unavailable. |

Before deploying against the production panel, compare its local changes with
the upstream commit above and verify that:

1. `ServerService::buildNodeConfig` returns the fields in the table without
   dropping `padding_scheme`, `transport`, `traffic_pattern`, or
   `tls_settings`.
2. Every subscription builder (Shadowrocket, Clash, ClashMeta, SingBox,
   Surge, and General) uses the user's UUID as the AnyTLS password and does
   not emit `skip-cert-verify`/`insecure` unless the administrator explicitly
   enabled the panel's allow-insecure option.
3. `UniProxyController::alive` passes an explicit empty array through to the
   device state service so a user's offline state is cleared.
4. Any panel-side report-ID/idempotent billing change is deployed as one
   transaction with its database receipt; otherwise leave the node's legacy
   report mode enabled.

The node implementation and tests in this repository are the compatible
patch. No production panel files were changed in the cloud environment.
