#!/usr/bin/env bash
set -euo pipefail
script="$(cd "$(dirname "$0")/../redis" && pwd)/device_reservation.lua"
tmp=$(mktemp -d)
sock="$tmp/redis.sock"
cleanup() { redis-cli -s "$sock" shutdown nosave >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT
redis-server --port 0 --unixsocket "$sock" --unixsocketperm 700 --save "" --appendonly no --daemonize yes --pidfile "$tmp/redis.pid" --logfile "$tmp/redis.log"
for i in $(seq 1 50); do
  if redis-cli -s "$sock" ping >/dev/null 2>&1; then break; fi
  sleep .1
done
call() { redis-cli --raw -s "$sock" --eval "$script" "$1" , "$2" "$3" "$4" "$5" "$6"; }
key=reservation:test:2
# Competing clients contend for two distinct IP slots.
for i in $(seq 1 32); do
  call "$key" acquire "192.0.2.$i" "node-$i" 2 60000 > "$tmp/result-$i" &
done
wait
test "$(grep -hx 1 "$tmp"/result-* | wc -l)" -eq 2
test "$(redis-cli -s "$sock" zcard "$key")" -eq 2
# Same IP on two nodes shares one device slot; release is owner-specific.
key=reservation:test:3
test "$(call "$key" acquire 192.0.2.1 owner-a 1 60000)" = 1
test "$(call "$key" acquire 192.0.2.1 owner-b 1 60000)" = 1
test "$(call "$key" release 192.0.2.1 owner-a 1 60000)" = 1
test "$(call "$key" acquire 192.0.2.2 owner-c 1 60000)" = 0
test "$(call "$key" renew 192.0.2.1 owner-a 1 60000)" = 0
test "$(call "$key" release 192.0.2.1 owner-b 1 60000)" = 1
test "$(call "$key" acquire 192.0.2.2 owner-c 1 1000)" = 1
sleep 1.2
test "$(call "$key" renew 192.0.2.2 owner-c 1 1000)" = 0
test "$(call "$key" acquire 192.0.2.3 owner-d 1 60000)" = 1
echo "Redis device reservation tests passed"
