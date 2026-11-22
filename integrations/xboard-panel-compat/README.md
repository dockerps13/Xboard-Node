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

The complete patch is directly applicable to that upstream commit. It changes
`ServerService`, `AbstractProtocol`, `ClientController`, adds the two
compatibility classes, and adds PHPUnit snapshot/unit coverage. Before
deploying against the production panel, compare its local changes with the
upstream commit above and verify that:

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

Apply and review it in a disposable or separately backed-up panel checkout:

```bash
git checkout d826a3b32f73bde24acce2a3ab033c6538ce121d
git apply --check /path/to/0001-xboard-node-v101-protocol-compat.patch
git apply /path/to/0001-xboard-node-v101-protocol-compat.patch
php -l app/Support/XboardNodeV101Compatibility.php
php -l app/Support/XboardNodeV101UnsupportedProtocolException.php
php artisan test tests/Unit/Support/XboardNodeV101CompatibilityTest.php
```

The patch's `AbstractProtocol` gate is inherited by Shadowrocket, ClashMeta,
SingBox, Surge, General, and the other protocol generators. It validates the
UUID credential and calls `assertSupported()` before any generator can drop a
node. Existing TLS builders preserve certificate verification by default and
only emit their client-specific insecure flag when the panel explicitly sets
`allow_insecure`. Unsupported combinations return HTTP 422 with protocol
names. To roll back this independent patch before deployment, run
`git apply -R` with the same patch after confirming the checkout has no
overlapping local edits.

The production checkout `/www/wwwroot/soulook.org` was not available to this
review and was not modified. Its local form customizations, database schema,
and deployed panel version remain unverified; compare them before applying.
