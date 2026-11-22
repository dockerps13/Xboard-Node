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
       } catch (\DomainException $e) {
           // An existing report ID cannot be reused with altered bytes.
           return response()->json(['message' => $e->getMessage()], 409);
       } catch (\Throwable $e) {
           // A failed DB/queue operation is not a durable receipt.
           \Illuminate\Support\Facades\Log::error('idempotent report failed', ['error' => $e->getMessage()]);
           return response()->json(['message' => 'report temporarily unavailable'], 503);
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
   panel. Run a separately configured `idempotent_traffic` queue worker with safe timeouts; the existing 30-second `traffic_fetch` worker cannot handle this job.
5. Add admin setting `server_idempotent_report_enable` with default `0`.
   Enable it ONLY after verifying DB transactions, queue retries, report
   receipts, report UUID uniqueness, and all three statistics in staging.

## Integer arithmetic and overflow acceptance

The opt-in Laravel worker uses `App\\Services\\IdempotentBillingMath` for overflow-checked raw traffic aggregation and rate-adjusted integer values. Each user's `u/d` debit and daily user statistics receive the same bounded integer amounts; a batch exceeding the PHP signed integer range throws and the enclosing DB transaction rolls back instead of marking the batch applied. Run `php integrations/xboard-idempotent/tests/billing_math.php` as an isolated boundary check. Rates with fractional multipliers truncate per user, consistent with the existing daily statistics implementation; verify this convention against the actual panel before enabling idempotent reports.

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

## Additional simulated multi-node billing safety test

From repository root run:

```bash
go test -count=1 ./internal/service -run '^TestTwoNodesLostBillingReceiptsRecoverWithoutDoubleCharge$'
```

This exercises two separate node-scoped disk journals, simultaneous HTTP submissions to a *simulated* shared idempotent ledger, lost receipts after commit and identical-ID replay after process restart. This does not verify Laravel balance changes.

## Dedicated queue Redis retry-after requirement

The verified production Redis queue sets `retry_after=90`, but the opt-in Laravel ledger job uses `timeout=180`. **A separate queue name on the same connection is insufficient.** All idempotent acceptance, recovery and job dispatches are pinned to a *separate* `redis_idempotent` queue connection. The staging panel must explicitly add this connection with `retry_after=240` and a dedicated Horizon supervisor with `timeout=210` before enabling the feature. Full safe copy-only snippets and verification gates: [docs/queue-configuration.md](docs/queue-configuration.md). Do **not** change the existing production queue, restart Horizon or enable idempotent reporting as part of this repository change.

## Production panel compatibility (read-only evidence from 2026-11-22)

Your `soulook.org` panel currently runs Laravel 12, PHP 8.2 and MySQL. The live `UserService::trafficFetch` applies `traffic.process.before` and `traffic.before_process`, then sends three **separate** queued jobs in chunks of 1,000 users: `TrafficFetchJob` debits user `u/d`, `StatUserJob` applies rated `u/d` to `v2_stat_user`, and `StatServerJob` records **raw** `u/d` in `v2_stat_server` and `v2_server`. The opt-in job must reproduce these exact semantics while atomically committing all three accounting effects and the ledger applied marker. The existing user-stat and server-stat tables have unique indexes **without** `record_type` (respectively `server_rate,user_id,record_at` and `server_id,server_type,record_at`). Never add conflicting `record_type` rows without checking these indexes.

**Critical queue mismatch discovered:** the existing production Horizon `data-pipeline` worker includes `traffic_fetch` with `timeout=30` seconds. The new combined ledger worker currently allows up to 180 seconds. It MUST NOT share this 30-second queue; it now routes to a dedicated `idempotent_traffic` queue that has **no running worker until explicitly configured**. Before enabling reports, configure an isolated PHP 8.2 Horizon supervisor which consumes ONLY `idempotent_traffic` using the `redis_idempotent` connection, with a worker timeout of 210 seconds and its own `retry_after=240` seconds. Check actual Horizon and queue defaults, deploy scheduler-based recovery and verify retry backoff, worker restarts and throughput. Do not add the new queue to an existing 30-second supervisor.

Until the dedicated worker and actual database tests exist, retain `server_idempotent_report_enable=0`; old nodes continue on their existing path. Never perform `artisan migrate` on the live database solely based on these reference instructions.

## Validation still needed before a commercial release

This integration has **not been installed or tested on your actual Vultr
panel**. Verify MySQL/MariaDB indexes on `v2_stat_user` and
`v2_stat_server`, plugins that hook traffic, multi-worker concurrency,
Redis failures, midnight rate windows, and backend queue timeouts.
Validate correct billing on a separate test machine ID with real clients.

The node Go tests simulate a ledger with an HTTP ACK loss. That simulation
is not proof that the Laravel integration works on the live panel.
