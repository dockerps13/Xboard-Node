<?php

declare(strict_types=1);

// Pure source contract test, intentionally independent of Laravel and MySQL.
// Does NOT replace real queue execution/transaction tests on a staging panel.
$root = dirname(__DIR__);
$paths = [
    'app/Jobs/ApplyIdempotentServerReportJob.php',
    'app/Services/IdempotentServerReportService.php',
    'app/Console/Commands/RecoverIdempotentServerReports.php',
];
foreach ($paths as $path) {
    $source = file_get_contents($root . '/' . $path);
    if ($source === false || !str_contains($source, "onConnection('redis_idempotent')->onQueue('idempotent_traffic')")) {
        throw new RuntimeException("Idempotent queue/connection binding missing from {$path}");
    }
}
$guide = file_get_contents($root . '/docs/queue-configuration.md');
if ($guide === false
    || !str_contains($guide, "'retry_after' => 240")
    || !str_contains($guide, "'timeout' => 210")
    || !str_contains($guide, "'connection' => 'redis_idempotent'")) {
    throw new RuntimeException('Isolated staging queue configuration is incomplete');
}
echo "PASS: idempotent billing queue wiring source contract\n";
