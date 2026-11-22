-- 忙期内保存整数请求数和起点，从同一个锚点算总泄漏，拒绝的请求不引入舍入累积。
local function advance(count, anchor, previous_us, now_us, capacity, units, period, compact)
    local as_of = math.max(previous_us, now_us)
    local elapsed = as_of - anchor
    if compact then
        -- 每个完整周期泄漏整数单位，移动锚点可限制长期运行时计数的大小。
        local periods = math.floor(elapsed / period)
        local removed = periods * units
        if removed >= count then
            count, anchor, elapsed = 0, as_of, 0
        elseif periods > 0 then
            count = count - removed
            anchor = anchor + periods * period
            elapsed = as_of - anchor
        end
    end
    local drained = elapsed * units
    if drained >= count * period then
        count, anchor, drained = 0, as_of, 0
    end
    -- 两侧同乘时间周期后比较；水位仅供展示，不作为下一次判定的输入。
    local allowed = drained >= (count + 1 - capacity) * period
    if allowed then count = count + 1 end
    local level = count - drained / period
    return allowed, level, as_of, count, anchor
end
