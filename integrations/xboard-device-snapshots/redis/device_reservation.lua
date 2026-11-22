-- KEYS[1]: per-user reservation ZSET, scoped to this panel.
-- ARGV: operation, canonical IP, owner token, device limit, lease ms.
-- Owner is a unique authenticated connection generation, not just node ID.
local op, ip, owner = ARGV[1], ARGV[2], ARGV[3]
local limit, ttl = tonumber(ARGV[4]), tonumber(ARGV[5])
if not ip or #ip == 0 or #ip > 45 or string.find(ip, '|', 1, true)
    or not owner or #owner == 0 or #owner > 128 or string.find(owner, '|', 1, true) then
    return redis.error_reply('invalid reservation identity')
end
if op ~= 'acquire' and op ~= 'renew' and op ~= 'release' then
    return redis.error_reply('invalid reservation operation')
end
if not limit or limit < 1 or limit ~= math.floor(limit) or not ttl or ttl < 1000 or ttl > 300000 or ttl ~= math.floor(ttl) then
    return redis.error_reply('invalid reservation parameters')
end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local key = KEYS[1]
local member = ip .. '|' .. owner
redis.call('ZREMRANGEBYSCORE', key, '-inf', now)
if op == 'release' then
    return redis.call('ZREM', key, member)
end
if op == 'renew' and not redis.call('ZSCORE', key, member) then
    return 0
end
if op == 'acquire' then
    local seen, count = {}, 0
    for _, entry in ipairs(redis.call('ZRANGE', key, 0, -1)) do
        local separator = string.find(entry, '|', 1, true)
        if not separator then return redis.error_reply('invalid stored reservation') end
        local addr = string.sub(entry, 1, separator - 1)
        if not seen[addr] then seen[addr] = true; count = count + 1 end
    end
    if not seen[ip] and count >= limit then return 0 end
end
redis.call('ZADD', key, now + ttl, member)
-- Preserve any longer existing lease, even if this request has a shorter TTL.
local last = redis.call('ZREVRANGE', key, 0, 0, 'WITHSCORES')
redis.call('PEXPIREAT', key, tonumber(last[2]) + 1000)
return 1
