-- Track the integer request count and the start of the busy period. Computing leakage from the same anchor prevents rejected requests from accumulating rounding error.
local function advance(count, anchor, previous_us, now_us, capacity, units, period, compact)
    local updated_at = math.max(previous_us, now_us)
    local elapsed_us = updated_at - anchor
    if compact then
        -- Each full period leaks whole units. Moving the anchor keeps the count bounded over time.
        local periods = math.floor(elapsed_us / period)
        local removed = periods * units
        if removed >= count then
            count, anchor, elapsed_us = 0, updated_at, 0
        elseif periods > 0 then
            count = count - removed
            anchor = anchor + periods * period
            elapsed_us = updated_at - anchor
        end
    end
    local leaked = elapsed_us * units
    if leaked >= count * period then
        count, anchor, leaked = 0, updated_at, 0
    end
    -- Multiply both sides by the period before comparing. The level is for display only.
    local allowed = leaked >= (count + 1 - capacity) * period
    local empty = count == 0
    if allowed then count = count + 1 end
    local level = count - leaked / period
    return allowed, level, updated_at, count, anchor, empty
end
