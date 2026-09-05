package distributed

// admissionScript is the single atomic composite admission script
// (ADR-018 D3/D4). It evaluates every enabled scope against one Redis TIME
// snapshot and commits request-cost tokens only when ALL enabled scopes allow;
// a rejection deducts nothing but still materializes refilled state and
// refreshes the participating keys' TTL, so a continuously rejected hot key
// never idle-expires into a fresh full bucket.
//
// Integer math rules followed here (mandatory invariant #3):
//   - one token = 60000 units; capacity = rpm*60000; request cost = 60000;
//   - refill term = elapsed_ms * rpm with elapsed clamped to [0, 60000], and
//     the addition is capped by capacity - tokens so tokens never exceed
//     capacity and no intermediate exceeds capacity < 2^53;
//   - the stored timestamp never moves backwards (monotonic clamp
//     effective_now = max(server_now, stored_last));
//   - the integer retry-after ceiling uses floor(deficit/rpm) plus a remainder
//     check; deficit <= cost = 60000 and rpm <= MaxSafeRPM keep the division
//     exact (the quotient cannot round across an integer boundary).
//
// Storage per scope: a Redis hash key with fields "t" (token units) and "s"
// (logical time ms). One shared PEXPIRE refresh happens on every admission
// (allow and reject) so idle state expires while active state persists.
const admissionScript = `
local COST = tonumber(ARGV[1])

local function server_now_ms()
    local t = redis.call('TIME')
    return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end

-- Evaluates and stages one scope. Writes refilled state + timestamp + TTL for
-- every call (allow and reject); deduction is applied later by the caller only
-- when the whole composite admission allows.
-- Returns allowed (1|0) and retry_after_ms (0 when allowed).
local function stage_scope(key, rpm, ttl, now)
    local cap = rpm * COST
    local row = redis.call('HMGET', key, 't', 's')
    local tokens = tonumber(row[1])
    local last = tonumber(row[2])
    if tokens == nil then
        tokens = cap
        last = now
    end
    -- Monotonic logical time: never move the stored timestamp backwards.
    if last > now then now = last end
    local elapsed = now - last
    if elapsed > 60000 then elapsed = 60000 end
    if elapsed < 0 then elapsed = 0 end
    -- Refill capped so tokens never exceed capacity and no intermediate can
    -- exceed the Lua exact-integer range.
    local room = cap - tokens
    local refill = elapsed * rpm
    if refill > room then refill = room end
    tokens = tokens + refill
    if tokens > cap then tokens = cap end
    -- Integer Lua numbers are passed to Redis commands directly (NO tostring):
    -- Redis 7.4.11's Lua bridge serializes integral numbers exactly (verified),
    -- while Lua tostring uses %.14g and turns e.g. 9007199254740000 into
    -- 9.00719925474e+15, which HINCRBY then rejects as a non-integer.
    redis.call('HSET', key, 't', tokens, 's', now)
    if ttl > 0 then redis.call('PEXPIRE', key, ttl) end
    if tokens >= COST then
        return 1, 0
    end
    local deficit = COST - tokens
    local q = math.floor(deficit / rpm)
    local retry = q
    if deficit - q * rpm > 0 then retry = retry + 1 end
    return 0, retry
end

local now = server_now_ms()
local allow = true
local retry = 0
local blocking = ''

if tonumber(ARGV[2]) == 1 then
    local ok, wait = stage_scope(KEYS[1], tonumber(ARGV[3]), tonumber(ARGV[4]), now)
    if ok ~= 1 then
        allow = false
        retry = wait
        blocking = 'vk'
    end
end
if tonumber(ARGV[5]) == 1 then
    local ok, wait = stage_scope(KEYS[2], tonumber(ARGV[6]), tonumber(ARGV[7]), now)
    if ok ~= 1 then
        if wait > retry then
            retry = wait
            blocking = 'proj'
        end
        allow = false
    end
end

if allow then
    -- All enabled scopes allow: atomically deduct the request cost from each.
    if tonumber(ARGV[2]) == 1 then
        redis.call('HINCRBY', KEYS[1], 't', -COST)
    end
    if tonumber(ARGV[5]) == 1 then
        redis.call('HINCRBY', KEYS[2], 't', -COST)
    end
    return {1, 0, ''}
end
return {0, retry, blocking}
`
