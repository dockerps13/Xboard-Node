<?php

namespace App\Services;

use App\Jobs\ApplyIdempotentServerReportJob;
use App\Models\Server;
use App\Services\Plugin\HookManager;
use Illuminate\Support\Facades\DB;
use DomainException;
use InvalidArgumentException;
use RuntimeException;

final class IdempotentServerReportService
{
    /**
     * Durable receipt: acknowledge only once the raw report and fixed rate
     * snapshot are committed. A worker applies the charge separately.
     */
    public function accept(Server $node, string $reportId, array $traffic): array
    {
        if (!preg_match('/\A[a-f0-9]{32}\z/D', $reportId)) {
            throw new InvalidArgumentException('invalid report_id');
        }
        if (!(bool) admin_setting('server_idempotent_report_enable', 0)) {
            throw new RuntimeException('idempotent report API is not enabled');
        }

        // Input bytes must be integers. Never multiply node traffic here:
        // XBoard jobs apply the rate once when processing the batch.
        $raw = self::normalize($traffic);
        if (!$raw) {
            throw new InvalidArgumentException('empty or invalid traffic batch');
        }
        ksort($raw, SORT_NUMERIC);
        $hash = hash('sha256', json_encode($raw, JSON_THROW_ON_ERROR));

        // Capture the time-dependent rate when first accepting a report.
        $server = $node->toArray();
        $server['rate'] = $node->getCurrentRate();
        $protocol = (string) $node->type;
        [$server, $protocol, $processed] = HookManager::filter('traffic.process.before', [$server, $protocol, $raw]);
        [$server, $protocol, $processed] = HookManager::filter('traffic.before_process', [$server, $protocol, $processed]);
        $processed = self::normalize($processed);
        if (!$processed) {
            throw new InvalidArgumentException('traffic hooks rejected all users');
        }

        $now = time();
        $recordId = DB::transaction(function () use ($node, $reportId, $hash, $processed, $server, $protocol, $now) {
            DB::table('v2_server_report_batches')->insertOrIgnore([
                'server_id' => (int) $node->id,
                'server_type' => (string) $node->type,
                'protocol' => (string) $protocol,
                'report_id' => $reportId,
                'payload_hash' => $hash,
                'payload_json' => json_encode($processed, JSON_THROW_ON_ERROR),
                'server_snapshot_json' => json_encode($server, JSON_THROW_ON_ERROR),
                'status' => 'pending',
                'record_at' => strtotime(date('Y-m-d', $now)),
                'applied_at' => null,
                'created_at' => $now,
                'updated_at' => $now,
            ]);
            $existing = DB::table('v2_server_report_batches')
                ->where('server_id', (int) $node->id)
                ->where('server_type', (string) $node->type)
                ->where('report_id', $reportId)
                ->lockForUpdate()
                ->first();
            if (!$existing || !hash_equals((string) $existing->payload_hash, $hash)) {
                // Same ID with altered bytes must never silently reuse a receipt.
                throw new DomainException('report_id already used with a different payload');
            }
            return (int) $existing->id;
        }, 3);

        // Dispatch after commit. If dispatch fails, retrying the same HTTP
        // request or running the pending-recovery command is safe.
        ApplyIdempotentServerReportJob::dispatch($recordId)->onQueue('traffic_fetch');

        return ['report_id' => $reportId, 'accepted' => true];
    }

    private static function normalize(array $traffic): array
    {
        $valid = [];
        foreach ($traffic as $uid => $counts) {
            if (!preg_match('/\A[1-9][0-9]*\z/D', (string) $uid)
                || !is_array($counts) || count($counts) !== 2
                || !isset($counts[0], $counts[1])
                || !is_int($counts[0]) || !is_int($counts[1])
                || $counts[0] < 0 || $counts[1] < 0) {
                throw new InvalidArgumentException('invalid uid or raw traffic counters');
            }
            $valid[(int) $uid] = [$counts[0], $counts[1]];
        }
        return $valid;
    }
}
