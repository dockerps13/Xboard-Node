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
        if (count($raw) > 20000) {
            throw new InvalidArgumentException('report has too many users');
        }
        ksort($raw, SORT_NUMERIC);
        $canonical = json_encode($raw, JSON_THROW_ON_ERROR);
        if (strlen($canonical) > 8 * 1024 * 1024) {
            throw new InvalidArgumentException('report exceeds 8 MiB payload limit');
        }
        $hash = hash('sha256', $canonical);

        // A replay must not run traffic hooks or capture a new rate. In
        // particular, a changed plugin configuration must not prevent a
        // node from obtaining the original durable receipt.
        $existing = DB::table('v2_server_report_batches')
            ->where('server_id', (int) $node->id)
            ->where('server_type', (string) $node->type)
            ->where('report_id', $reportId)
            ->first();
        if ($existing) {
            if (!hash_equals((string) $existing->payload_hash, $hash)) {
                throw new DomainException('report_id already used with a different payload');
            }
            if ($existing->status === 'pending') {
                ApplyIdempotentServerReportJob::dispatch((int) $existing->id)->onConnection('redis_idempotent')->onQueue('idempotent_traffic');
            }
            return ['report_id' => $reportId, 'accepted' => true];
        }

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
        if (count($processed) > 20000 || strlen(json_encode($processed, JSON_THROW_ON_ERROR)) > 8 * 1024 * 1024) {
            throw new InvalidArgumentException('processed traffic exceeds batch limits');
        }
        $rate = $server['rate'] ?? null;
        if (!is_numeric($rate) || !is_finite((float) $rate) || (float) $rate < 0) {
            throw new InvalidArgumentException('invalid effective traffic rate');
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
        ApplyIdempotentServerReportJob::dispatch($recordId)->onConnection('redis_idempotent')->onQueue('idempotent_traffic');

        return ['report_id' => $reportId, 'accepted' => true];
    }

    private static function normalize(array $traffic): array
    {
        $valid = [];
        foreach ($traffic as $uid => $counts) {
            if (!preg_match('/\A[1-9][0-9]*\z/D', (string) $uid)
                || (string) (int) $uid !== (string) $uid
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
