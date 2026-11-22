# Isolated billing Redis connection (staging only)

The real `soulook.org` production panel was inspected read-only on 2026-11-22:

- Laravel 12.54.1, PHP 8.2, Redis queue.
- Existing `config/queue.php`: `redis.retry_after = 90`.
- Production Horizon `data-pipeline`: `traffic_fetch`, `stat`, `user_alive_sync`; timeout 30 seconds.
- Opt-in idempotent ledger job: timeout 180 seconds.

**Do not assign the new job to the existing `redis` connection.** Merely changing the queue name does not change `retry_after`. A 180-second task may be made visible for another worker after 90 seconds, before the first worker exits. SQL ledger locking mitigates duplicate debits but does not fix retries, worker churn and stalled settlements.

## Required staging-only wiring

In the **test copy** of the panel's `config/queue.php`, inside the existing `connections` array, add a second queue connection, reusing the existing Redis client connection and database. Never remove or modify the existing `redis` entry:

```php
'redis_idempotent' => [
    'driver' => 'redis',
    'connection' => 'default',
    'queue' => 'idempotent_traffic',
    'retry_after' => 240,
    'block_for' => null,
],
```

In the **test copy** of `config/horizon.php`, under `environments.production`, add a **new** supervisor, without changing `data-pipeline`, `business` or `notification`:

```php
'idempotent-billing' => [
    'connection' => 'redis_idempotent',
    'queue' => ['idempotent_traffic'],
    'balance' => 'simple',
    'minProcesses' => 1,
    'maxProcesses' => 1,
    'tries' => 12,
    'timeout' => 210,
    'memory' => 256,
],
```

With `job timeout=180 < Horizon worker timeout=210 < retry_after=240`, a normal job will not become available for a second worker while still running. Set the actual `retry_after` strictly **greater** than the worker timeout, including deployment differences. `job timeout` might be enforced earlier than Horizon's timeout; both should be checked during staging.

The new job constructor, request-side dispatch and recovery scheduler all explicitly use:

```php
->onConnection('redis_idempotent')->onQueue('idempotent_traffic')
```

Only deploy after a verified isolated DB/source backup and validation of queue worker visibility and DB transactions. The handshaked flag `server_idempotent_report_enable` stays **0** until completion. Never run live `php artisan migrate` solely from this guide.

## Staging checks (must not be executed on production without approval)

- `/www/server/php/82/bin/php artisan config:clear` **on the staging copy only**, then inspect the effective connection values and Horizon supervisors.
- Ensure the test Horizon process runs PHP 8.2, and the dedicated supervisor consumes `idempotent_traffic` from `redis_idempotent`.
- Validate lost ACK, multiple workers, duplicate report IDs, repeated retries, DB deadlocks, queue restart, and a dropped Redis notification; verify user `u/d`, daily stats, raw server stats, node totals and ledger status in a **test database**.
- Check hooks, midnight attribution, real MySQL unique keys and queue-specific `retry_after`.
- If queues cannot be safely separated with the current Redis deployment, do not enable the opt-in feature.

The existing legacy XBoard flow is unchanged.
