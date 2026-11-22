<?php

namespace App\Console\Commands;

use App\Jobs\ApplyIdempotentServerReportJob;
use App\Models\ServerReportBatch;
use Illuminate\Console\Command;

class RecoverIdempotentServerReports extends Command
{
    protected $signature = 'traffic:recover-idempotent {--limit=500} {--min-age=300}';
    protected $description = 'Requeue durable XBoard reports not yet applied to user billing';

    public function handle(): int
    {
        $limit = min(10000, max(1, (int) $this->option('limit')));
        $age = max(60, (int) $this->option('min-age'));
        $count = 0;
        // Do not repeatedly dispatch freshly accepted batches while the
        // original queue worker is still processing them.
        ServerReportBatch::query()->where('status', 'pending')
            ->where('created_at', '<=', time() - $age)
            ->orderBy('id')
            ->limit($limit)
            ->pluck('id')
            ->each(function ($id) use (&$count) {
                ApplyIdempotentServerReportJob::dispatch((int) $id)
                    ->onConnection('redis_idempotent')->onQueue('idempotent_traffic');
                $count++;
            });

        $this->info("Requeued {$count} pending report batch(es)");
        return self::SUCCESS;
    }
}
