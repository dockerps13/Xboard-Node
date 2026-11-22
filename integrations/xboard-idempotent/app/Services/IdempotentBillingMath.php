<?php

namespace App\Services;

use InvalidArgumentException;
use OverflowException;

/**
 * Safe integer arithmetic for raw and rate-adjusted traffic counters.
 * This helper deliberately contains no Laravel dependencies so billing
 * boundary cases can be tested before installing the panel integration.
 */
final class IdempotentBillingMath
{
    public static function addRaw(int $current, int $bytes): int
    {
        if ($current < 0 || $bytes < 0) {
            throw new InvalidArgumentException('negative traffic counter');
        }
        if ($current > PHP_INT_MAX - $bytes) {
            throw new OverflowException('summed raw traffic exceeds integer range');
        }
        return $current + $bytes;
    }

    public static function chargedBytes(int $bytes, float $rate): int
    {
        if ($bytes < 0 || !is_finite($rate) || $rate < 0) {
            throw new InvalidArgumentException('invalid traffic or node rate');
        }
        $result = $bytes * $rate;
        // The integer limit rounds upward at float64 precision. Reject the
        // boundary conservatively instead of risking a platform-dependent cast.
        if (!is_finite($result) || $result >= (float) PHP_INT_MAX) {
            throw new OverflowException('rated traffic exceeds integer range');
        }
        return (int) $result; // Matches the existing statistics truncation.
    }
}
