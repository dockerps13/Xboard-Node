<?php
require __DIR__ . '/../app/Services/GlobalDeviceSnapshot.php';

use App\Services\GlobalDeviceSnapshot;

function check($condition, string $message): void
{
    if (!$condition) {
        throw new RuntimeException($message);
    }
}

$called = [];
$result = GlobalDeviceSnapshot::build([2, '2', 3], function ($ids) use (&$called) {
    $called = $ids;
    return [
        2 => ['192.0.2.1', '::ffff:192.0.2.1', '2001:0db8::1', '2001:db8::1', 'invalid'],
        3 => [],
        99 => ['192.0.2.99'],
    ];
});
check($called === [2, 3], 'authorized IDs not deduplicated');
check(json_encode($result) === '{"users":{"2":["192.0.2.1","2001:db8::1"]}}', 'normalization or authorization failed');
check(json_encode(GlobalDeviceSnapshot::build([], function () { throw new RuntimeException('unexpected read'); })) === '{"users":{}}', 'empty snapshot shape');
try {
    GlobalDeviceSnapshot::build([2], function () { throw new RuntimeException('Redis unavailable'); });
    throw new LogicException('backend failure swallowed');
} catch (RuntimeException $e) {
    check($e->getMessage() === 'Redis unavailable', 'wrong error');
}
try {
    GlobalDeviceSnapshot::build([0], function () { return []; });
    throw new LogicException('invalid ID accepted');
} catch (InvalidArgumentException $e) {}
echo "Device snapshot tests passed\n";
