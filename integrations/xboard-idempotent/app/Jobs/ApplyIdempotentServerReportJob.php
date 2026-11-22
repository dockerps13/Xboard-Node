<?php

namespace App\Jobs;

use App\Models\ServerReportBatch;
use App\Models\StatServer;
use App\Models\StatUser;
use App\Models\User;
use App\Services\IdempotentBillingMath;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Redis;
use RuntimeException;

/**
 * MySQL/MariaDB-only reference implementation.
 * User debit, user stats, server stats and the applied marker commit in the
 * SAME DB transaction. Duplicate execution sees the applied marker and
 * cannot increment twice.
 */
class ApplyIdempotentServerReportJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public int $tries = 12;
    public int $timeout = 180;

    public function __construct(public int $batchId)
    {
        $this->onConnection('redis_idempotent')->onQueue('idempotent_traffic');
    }

    public function backoff(): array
    {
        return [1, 5, 15, 30, 60];
    }

    public function handle(): void
    {
        if (!in_array(config('database.default'), ['mysql', 'mariadb'], true)) {
            throw new RuntimeException('idempotent report processor requires MySQL/MariaDB');
        }

        $userIds = DB::transaction(function () {
            $batch = ServerReportBatch::whereKey($this->batchId)
                ->lockForUpdate()
                ->firstOrFail();

            $data = json_decode($batch->payload_json, true, 512, JSON_THROW_ON_ERROR);
            $server = json_decode($batch->server_snapshot_json, true, 512, JSON_THROW_ON_ERROR);
            $users = array_map('intval', array_keys($data));

            if ($batch->status === 'applied') {
                // Retry the post-commit Redis pending-check notification.
                return $users;
            }
            if ($batch->status !== 'pending') {
                throw new RuntimeException('unknown report ledger status');
            }
            $rate = (float) ($server['rate'] ?? 0);
            if (!is_finite($rate) || $rate < 0) {
                throw new RuntimeException('invalid stored node rate');
            }

            $now = time();
            $statRows = [];
            $serverUp = 0;
            $serverDown = 0;
            foreach ($data as $uid => $counts) {
                $uid = (int) $uid;
                $up = (int) $counts[0];
                $down = (int) $counts[1];
                $serverUp = IdempotentBillingMath::addRaw($serverUp, $up);
                $serverDown = IdempotentBillingMath::addRaw($serverDown, $down);
                $chargedUp = IdempotentBillingMath::chargedBytes($up, $rate);
                $chargedDown = IdempotentBillingMath::chargedBytes($down, $rate);

                // Keep user balance and daily stats on the exact same integer
                // counters; reject overflow before executing the transaction.
                User::where('id', $uid)->incrementEach([
                    'u' => $chargedUp,
                    'd' => $chargedDown,
                ], ['t' => $now]);

                $statRows[] = [
                    'user_id' => $uid,
                    'server_rate' => $rate,
                    'record_at' => (int) $batch->record_at,
                    'record_type' => 'd',
                    'u' => $chargedUp,
                    'd' => $chargedDown,
                    'created_at' => $now,
                    'updated_at' => $now,
                ];
            }

            foreach (array_chunk($statRows, 500) as $rows) {
                StatUser::upsert(
                    $rows,
                    ['user_id', 'server_rate', 'record_at', 'record_type'],
                    [
                        'u' => DB::raw('u + VALUES(u)'),
                        'd' => DB::raw('d + VALUES(d)'),
                        'updated_at' => $now,
                    ]
                );
            }

            StatServer::upsert([
                'record_at' => (int) $batch->record_at,
                'server_id' => (int) $batch->server_id,
                'server_type' => (string) $batch->protocol,
                'record_type' => 'd',
                'u' => $serverUp,
                'd' => $serverDown,
                'created_at' => $now,
                'updated_at' => $now,
            ], ['server_id', 'server_type', 'record_at', 'record_type'], [
                'u' => DB::raw('u + VALUES(u)'),
                'd' => DB::raw('d + VALUES(d)'),
                'updated_at' => $now,
            ]);

            DB::table('v2_server')
                ->where('id', (int) $batch->server_id)
                ->incrementEach(['u' => $serverUp, 'd' => $serverDown], ['updated_at' => now()]);

            $batch->status = 'applied';
            $batch->applied_at = $now;
            $batch->updated_at = $now;
            $batch->save();

            return $users;
        }, 3);

        // A Redis failure after commit is safe to retry: the DB ledger
        // suppresses a second debit, and pending_check is an idempotent SET.
        if ($userIds) {
            Redis::sadd('traffic:pending_check', ...$userIds);
        }
    }
}
