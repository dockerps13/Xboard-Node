<?php

declare(strict_types=1);

require __DIR__ . '/../app/Services/IdempotentBillingMath.php';

use App\Services\IdempotentBillingMath;

function assertSameValue($want, $got, string $case): void
{
    if ($got !== $want) {
        throw new RuntimeException("{$case}: expected " . var_export($want, true) . ", got " . var_export($got, true));
    }
}

function expectException(callable $fn, string $type, string $case): void
{
    try {
        $fn();
    } catch (Throwable $error) {
        if ($error instanceof $type) {
            return;
        }
        throw new RuntimeException("{$case}: unexpected " . get_class($error), 0, $error);
    }
    throw new RuntimeException("{$case}: expected {$type}");
}

assertSameValue(300, IdempotentBillingMath::addRaw(100, 200), 'raw sum');
assertSameValue(PHP_INT_MAX, IdempotentBillingMath::addRaw(PHP_INT_MAX - 1, 1), 'raw boundary');
expectException(fn () => IdempotentBillingMath::addRaw(PHP_INT_MAX, 1), OverflowException::class, 'raw overflow');
expectException(fn () => IdempotentBillingMath::addRaw(0, -1), InvalidArgumentException::class, 'negative raw');
assertSameValue(7, IdempotentBillingMath::chargedBytes(5, 1.5), 'rated truncation');
assertSameValue(0, IdempotentBillingMath::chargedBytes(5, 0.0), 'zero rate');
expectException(fn () => IdempotentBillingMath::chargedBytes(PHP_INT_MAX, 2.0), OverflowException::class, 'rated overflow');
expectException(fn () => IdempotentBillingMath::chargedBytes(1, INF), InvalidArgumentException::class, 'infinite rate');
expectException(fn () => IdempotentBillingMath::chargedBytes(1, NAN), InvalidArgumentException::class, 'NaN rate');
expectException(fn () => IdempotentBillingMath::chargedBytes(-1, 1.0), InvalidArgumentException::class, 'negative rated');
expectException(fn () => IdempotentBillingMath::chargedBytes(1, -1.0), InvalidArgumentException::class, 'negative rate');

echo "PASS: isolated idempotent billing math guards\n";
