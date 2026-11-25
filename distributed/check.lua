-- Read Redis time and bucket state, make the decision, and commit it in one atomic execution.
local clock = redis.call('TIME')
local now_us = tonumber(clock[1]) * 1000000 + tonumber(clock[2])
local previous = redis.call('HMGET', KEYS[1], 'count', 'anchor_us', 'at_us',
    'capacity', 'units', 'period', 'compact', 'leak_rate')
-- Active buckets keep their original policy. A replica with the original settings fills in a missing legacy policy on the first check.
local policy = ARGV
if previous[4] then policy = {unpack(previous, 4, 8)} end
local allowed, level, updated_at, count, anchor, empty = advance(
    tonumber(previous[1]) or 0, tonumber(previous[2]) or now_us,
    tonumber(previous[3]) or now_us, now_us,
    tonumber(policy[1]), tonumber(policy[2]), tonumber(policy[3]), policy[4] == '1')
-- Expiry rounding can keep an empty key around for less than a millisecond. The next request can already adopt the new policy.
if empty then policy = ARGV end
local water = string.format('%.17g', level)
local timestamp = string.format('%.0f', updated_at)
redis.call('HSET', KEYS[1], 'level', water, 'at_us', timestamp,
    'count', string.format('%.0f', count), 'anchor_us', string.format('%.0f', anchor),
    'capacity', policy[1], 'units', policy[2], 'period', policy[3],
    'compact', policy[4], 'leak_rate', policy[5])

-- Redis expiry has millisecond precision. Separate the large integer timestamp from the fractional offset before rounding up.
local empty_after_us = count / tonumber(policy[2]) * tonumber(policy[3])
local expires_at_ms = math.floor(anchor / 1000) + math.ceil((anchor % 1000 + empty_after_us) / 1000)
if expires_at_ms <= 9007199254740991 then
    redis.call('PEXPIREAT', KEYS[1], string.format('%.0f', expires_at_ms))
else
    -- Very low leak rates can put expiry beyond Lua's exact integer range. Keep the key to avoid releasing quota too early.
    redis.call('PERSIST', KEYS[1])
end
return {allowed and 1 or 0, water, timestamp, policy[1], policy[5]}
