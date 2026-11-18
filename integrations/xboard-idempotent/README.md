# XBoard idempotent traffic reporting integration (opt-in)

> **NOT installed on soulook.org.** This is a drop-in integration candidate for
> the XBoard PHP panel. Do not enable the setting until tested against a
> backed-up, isolated XBoard database. Requires MySQL/MariaDB and the actual
> XBoard schema used by `soulook.org`.

## Provided components

- Migration: durable batch ledger with unique
  `(server_id, server_type, report_id)` and receipt-time rate snapshot
- `IdempotentServerReportService`: validate raw byte counters, retain the
  existing traffic hooks, commit the batch, return the durable acceptance
- `ApplyIdempotentServerReportJob`: one DB transaction for user debit,
  daily user statistics, raw server statistics, server totals, and the
  `applied` marker; repeat executions cannot apply the same batch twice
- `RecoverIdempotentServerReports`: redispatch any persisted pending batches
  after queue failures

Existing legacy nodes use the original asynchronous reporting code path.
Only nodes that see `idempotent_reports=true` in a V2 handshake use the
new report_id protocol.

## Required panel wiring (after copying files into the Laravel project)

1. Take a verified database + source backup. Copy the provided `app/` and
   `database/` files into the matching Laravel directories, then run
   `php artisan migrate` on a test copy only.
2. In `ServerController::handshake()`, add `settings` beside `websocket`
   in the JSON result. Preserve existing interval settings:

   ```php
   'settings' => [
       'push_interval' => (int) admin_setting('server_push_interval', 60),
       'pull_interval' => (int) admin_setting('server_pull_interval', 60),
       'idempotent_reports' => (bool) admin_setting('server_idempotent_report_enable', 0),
   ],
   ```

3. In `ServerController::report()`, replace only the traffic branch:

   ```php
   $receipt = null;
   $traffic = $request->input('traffic');
   if ($request->exists('report_id')) {
       try {
           $receipt = app(\App\Services\IdempotentServerReportService::class)
               ->accept($node, (string) $request->input('report_id'), (array) $traffic);
           // Match legacy ServerService::processTraffic cache side effects.
           $nodeType = strtoupper((string) $node->type);
           \Illuminate\Support\Facades\Cache::put(
               \App\Utils\CacheKey::get("SERVER_{$nodeType}_ONLINE_USER", $node->id),
               count((array) $traffic), 3600
           );
           \Illuminate\Support\Facades\Cache::put(
               \App\Utils\CacheKey::get("SERVER_{$nodeType}_LAST_PUSH_AT", $node->id),
               time(), 3600
           );
       } catch (\InvalidArgumentException $e) {
           return response()->json(['message' => $e->getMessage()], 422);
       } catch (\RuntimeException $e) {
           // Includes an ID reused with altered payload; never fall back
           // to legacy billing and never return accepted=true.
           return response()->json(['message' => $e->getMessage()], 409);
       }
   } elseif (is_array($traffic) && !empty($traffic)) {
       ServerService::processTraffic($node, $traffic);
   }
   ```

   Leave `processAlive`, `processOnline`, `processStatus`,
   `updateMetrics`, authentication middleware and `touchNode` intact.
   Replace the final response with
   `return response()->json(['data' => $receipt ?? true]);`.
4. Register `RecoverIdempotentServerReports` as an Artisan command if this
   Laravel version does not auto-discover `app/Console/Commands`. Schedule
   `php artisan traffic:recover-idempotent` once per minute on the test
   panel. Keep a `traffic_fetch` queue worker running.
5. Add admin setting `server_idempotent_report_enable` with default `0`.
   Enable it ONLY after verifying DB transactions, queue retries, report
   receipts, report UUID uniqueness, and all three statistics in staging.

## Invariants

- A batch is acknowledged only after the ledger insert commits.
- A duplicate `report_id` with unchanged raw bytes receives the same
  accepted receipt without triggering duplicate billing.
- The same ID with changed bytes yields an HTTP conflict.
- The worker locks the ledger row, updates all DB counters, and marks the
  row `applied` atomically; worker retries cannot double-charge.
- The node replays a persisted batch with its **original report_id** only.
- The recorded rate is chosen on first acceptance, not recomputed during
  delayed queue processing. Daily statistics use the stored receipt date.
- Legacy nodes remain unaffected.

## Validation still needed before a commercial release

This integration has **not been installed or tested on your actual Vultr
panel**. Verify MySQL/MariaDB indexes on `v2_stat_user` and
`v2_stat_server`, plugins that hook traffic, multi-worker concurrency,
Redis failures, midnight rate windows, and backend queue timeouts.
Validate correct billing on a separate test machine ID with real clients.

The node Go tests simulate a ledger with an HTTP ACK loss. That simulation
is not proof that the Laravel integration works on the live panel.
