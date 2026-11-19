# Optional REST global device snapshots

Not installed on the live panel. This is a proposed opt-in protocol, not an assertion that stock XBoard has this endpoint.

Advertise settings.global_device_snapshots=true in the V2 handshake only after implementing POST /api/v2/server/devices with the same node authentication and authorization as the V2 report endpoint. Return {"users":{"2":["192.0.2.1"]}}; return {"users":{}} for a successful empty snapshot. Include only users authorized for the requesting node and their distinct, unexpired device IPs across nodes. Do not return every panel user or expired Redis entries. Set Cache-Control: no-store. Derive node identity from validated credentials and node binding, not arbitrary client input.

The node fetches the full snapshot at bootstrap and REST polling, independent of user/config ETags. Failure does not clear the last snapshot; kernel freshness limits still expire it after 60 seconds. A connected WebSocket takes precedence. Legacy panels that omit the capability receive no additional requests.

This is eventual synchronization, not strict distributed admission. A central atomic lease/reservation API and concurrency tests are required for strict multi-server device limits. Test the actual Laravel controller, authorization, expiration, and Redis schema before enabling. No panel code or production deployment is included here.

## PHP response component

Copy app/Services/GlobalDeviceSnapshot.php to the panel test copy. Inside the authenticated V2 controller, reuse the exact existing user endpoint authorization logic to produce the allowed user IDs, then call:

```php
$payload = \\App\\Services\\GlobalDeviceSnapshot::build(
    $authorizedUserIds,
    fn (array $ids) => app(\\App\\Services\\DeviceStateService::class)->getUsersDevices($ids)
);
return response()->json($payload)->header('Cache-Control', 'no-store');
```

The existing DeviceStateService must return user ID => list of unexpired IP strings. Verify this contract against the deployed source. Return an error on Redis failure rather than an empty successful result. The helper filters unauthorized rows even if the callback returns extra users. The exact route and middleware wiring must be verified on the real panel before enabling the handshake flag. Run php integrations/xboard-device-snapshots/tests/snapshot.php for standalone regression tests.
