-- Redis 时间、读桶、判定和提交共用一次原子执行。
local clock = redis.call('TIME')
local now_us = tonumber(clock[1]) * 1000000 + tonumber(clock[2])
local capacity, units, period = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local previous = redis.call('HMGET', KEYS[1], 'count', 'anchor_us', 'at_us')
local allowed, level, as_of, count, anchor = advance(
    tonumber(previous[1]) or 0, tonumber(previous[2]) or now_us,
    tonumber(previous[3]) or now_us, now_us, capacity, units, period, ARGV[4] == '1')
local water = string.format('%.17g', level)
local timestamp = string.format('%.0f', as_of)
redis.call('HSET', KEYS[1], 'level', water, 'at_us', timestamp,
    'count', string.format('%.0f', count), 'anchor_us', string.format('%.0f', anchor))

-- Redis 精确到毫秒。截止时间向上取整，且先分离大整数时间戳与小数部分。
local empty_after_us = count / units * period
local expires_at_ms = math.floor(anchor / 1000) + math.ceil((anchor % 1000 + empty_after_us) / 1000)
if expires_at_ms <= 9007199254740991 then
    redis.call('PEXPIREAT', KEYS[1], string.format('%.0f', expires_at_ms))
else
    -- 极小漏速可能超出 Lua 整数精度，此时保留桶可以避免提前放行。
    redis.call('PERSIST', KEYS[1])
end
return {allowed and 1 or 0, water, timestamp}
