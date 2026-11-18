<?php

namespace App\Console\Commands;

use App\Jobs\ApplyIdempotentServerReportJob;
use App\Models\ServerReportBatch;
use Illuminate\Console\Command;

class RecoverIdempotentServerReports extends Command
{
    protected $signature = 'traffic:recover-idempotent {--limit=500}';
    protected $description = 'Requeue durable XBoard reports not yet applied to user billing';

    public function handle(): int
    {
        $limit = min(10000, max(1, (int) $this->option('limit')));
        $count = 0;
        ServerReportBatch::query()->where('status', 'pending')
            ->orderBy('id')
            ->limit($limit)
            ->pluck('id')
            ->each(function ($id) use (&$count) {
                ApplyIdempotentServerReportJob::dispatch((int) $id)
                    ->onQueue('traffic_fetch');
                $count++;
            });

        $this->info("Requeued {$count} pending report batch(es)");
        return self::SUCCESS;
    }
}
