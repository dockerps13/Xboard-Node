<?php

namespace App\Services;

use InvalidArgumentException;

/**
 * REST snapshot response builder. The caller MUST obtain authorized IDs
 * using the same authenticated node/user selection as the V2 user endpoint.
 * Never pass user IDs supplied by the requesting node.
 */
final class GlobalDeviceSnapshot
{
    public static function build(array $authorizedIds, callable $readDevices): array
    {
        $ids = [];
        foreach ($authorizedIds as $id) {
            if ((!is_int($id) && !(is_string($id) && ctype_digit($id))) || (int) $id <= 0) {
                throw new InvalidArgumentException('Invalid authorized user ID');
            }
            $ids[(int) $id] = true;
        }

        $out = [];
        if ($ids !== []) {
            // The existing DeviceStateService applies per-device expiration.
            // Let backend failures propagate: they are not empty snapshots.
            $devices = $readDevices(array_keys($ids));
            if (!is_array($devices)) {
                throw new InvalidArgumentException('Invalid device state');
            }
            foreach ($devices as $id => $addresses) {
                if (!isset($ids[$id])) {
                    continue;
                }
                if (!is_array($addresses)) {
                    throw new InvalidArgumentException('Invalid device addresses');
                }
                $unique = [];
                foreach ($addresses as $address) {
                    if (!is_string($address)) {
                        continue;
                    }
                    $packed = @inet_pton($address);
                    if ($packed === false) {
                        continue;
                    }
                    if (strlen($packed) === 16 && substr($packed, 0, 12) === str_repeat("\0", 10) . "\xff\xff") {
                        $packed = substr($packed, 12);
                    }
                    $ip = inet_ntop($packed);
                    if ($ip !== false) {
                        $unique[$ip] = true;
                    }
                }
                if ($unique !== []) {
                    $ips = array_keys($unique);
                    sort($ips, SORT_STRING);
                    $out[(int) $id] = $ips;
                }
            }
        }
        return ['users' => (object) $out];
    }
}
