package cache

import "github.com/redis/go-redis/v9"

// Lua 脚本集中在这里，判断标准只有一条：
//
//	两条以上写命令之间存在因果依赖，且中间失败会留下「不可自愈」的脏状态。
//
// Pipeline 只省往返，既不是事务，也不保证全部执行：断连、超时、单条命令报错
// 都可能让前半截生效、后半截丢失。这类场景必须交给 Lua 一次执行。
var (
	// markReadScript 上报已读：位置只前进，并同步清空未读计数、续期已读 hash。
	// KEYS[1] = im:read:{uid}    KEYS[2] = im:unread:{uid}
	// ARGV[1] = convID           ARGV[2] = seq       ARGV[3] = read TTL(ms)
	// 返回 {生效后的 seq, 是否前进}
	//
	// 不用裸 HSET 的原因：多端或弱网下上报会乱序，无条件的 HSET 回退
	// 会让群未读 = last_seq - read_seq 突然变大，DB 侧有 GREATEST、
	// Redis 侧没有，两边立刻不一致。
	//
	// PEXPIRE 必须和 HSET 同脚本：拆开写一旦续期丢失，hash 就变回永久键，
	// 一个用户一条、field 数等于他的会话数，随用户数单调增长且无人回收。
	// 这里对「重复上报、未前进」的调用同样续期：只要用户还在上报，说明会话仍然活跃，
	// 与是否推进已读位置无关。
	markReadScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], ARGV[1])
local seq = tonumber(ARGV[2])
local advanced = 0
if cur == false or tonumber(cur) < seq then
    redis.call('HSET', KEYS[1], ARGV[1], seq)
    advanced = 1
else
    seq = tonumber(cur)
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return {seq, advanced}
`)

	// incrUnreadScript 批量增加未读数并给每个 key 续期。
	// KEYS = im:unread:{uid} 列表
	// ARGV[1] = convID  ARGV[2] = delta  ARGV[3] = TTL(ms)
	//
	// 为什么不用 pipeline：HINCRBY 成功而 PEXPIRE 丢失，这个用户的未读 hash
	// 就成了永久键 —— 一个用户一条、field 数等于他的会话数，随用户数单调增长
	// 且没有任何回收路径。pipeline 只省往返，不保证全部执行。
	//
	// 脚本里没有额外判断：HINCRBY 本身原子，这里要保证的只是「计数与续期同生」，
	// 一条脚本包住两条命令就够，不必堆复杂逻辑。
	//
	// 多 Key 脚本在 Redis Cluster 下会 CROSSSLOT（不同 uid 落在不同槽），当前用单机
	// 客户端没这个问题；将来换 Cluster 客户端必须改成按 uid 分批或退回 pipeline。
	incrUnreadScript = redis.NewScript(`
local conv = ARGV[1]
local delta = tonumber(ARGV[2])
local ttl = ARGV[3]
for i = 1, #KEYS do
    redis.call('HINCRBY', KEYS[i], conv, delta)
    redis.call('PEXPIRE', KEYS[i], ttl)
end
return #KEYS
`)

	// slideWindowScript 滑动窗口限流，返回 {窗口内已用次数, 重试等待毫秒, 是否放行}。
	// KEYS[1] = zset 键
	// ARGV[1] = 当前时间戳(ms)  ARGV[2] = 窗口毫秒
	// ARGV[3] = 本次请求的唯一成员  ARGV[4] = 窗口上限
	//
	// 为什么不用固定窗口：固定窗口在窗口边界会漏出双倍额度 —— 第 59 秒打满
	// 一个窗口、第 61 秒再打满下一个，两秒内实际通过 2×limit，对登录撞库
	// 这种场景等于限流形同虚设。滑动窗口用 score 存时间戳，按「最近一个窗口」
	// 统计，边界处不会出现这种突刺。
	//
	// 拒绝路径不写成员：被刷的子请求不该继续占用内存，否则攻击者能把一个
	// 键撑到无限大。ZREMRANGEBYSCORE 顺手把滑出窗口的旧记录清掉，
	// 键的大小稳定在 limit 量级，配合 PEXPIRE 兜底回收。
	//
	// 判断放在脚本里而不是返回计数再由 Go 比较：先判后写才能保证
	// 「同一时刻只有 limit 个请求看到放行」，分两步会超发。
	slideWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local win = tonumber(ARGV[2])
local limit = tonumber(ARGV[4])

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - win)

local n = redis.call('ZCARD', key)
if n >= limit then
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local retry = win
    if oldest[2] then
        retry = tonumber(oldest[2]) + win - now
    end
    if retry < 1 then
        retry = 1
    end
    redis.call('PEXPIRE', key, win)
    return {n, retry, 0}
end

redis.call('ZADD', key, now, ARGV[3])
redis.call('PEXPIRE', key, win)
return {n + 1, 0, 1}
`)

	// incrFailScript 登录失败计数。
	// KEYS[1] = 计数键  ARGV[1] = 窗口毫秒
	//
	// 每次失败都续期，让「锁定 5 分钟」从最后一次失败算起。
	// 这里的原子性不是可有可无的：一旦 INCR 成功而 PEXPIRE 丢失，
	// 计数会停在阈值上且永不过期，而读路径发现超阈值就直接返回 ——
	// 后续不会再有任何失败请求来补 TTL，这个账号就被永久锁死了。
	incrFailScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return n
`)

	// setOnlineScript 记录设备在线。
	// KEYS[1] = im:online:{uid}
	// ARGV[1] = device  ARGV[2] = 该设备过期时间戳(ms)
	// ARGV[3] = 当前时间戳(ms)  ARGV[4] = key 级 TTL(ms)
	//
	// 用 zset 的 score 存「设备自身的过期时刻」，而不是 set + key 级 TTL：
	// 多端共用一个 key 时，任一设备心跳都会刷新整个 key 的 TTL，
	// 已断开设备的成员就会被无限期保留，导致 IsOnline 恒为 true。
	setOnlineScript = redis.NewScript(`
redis.call('ZADD', KEYS[1], tonumber(ARGV[2]), ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[3])
if redis.call('ZCARD', KEYS[1]) > 0 then
    redis.call('PEXPIRE', KEYS[1], ARGV[4])
end
return 1
`)

	// pushFeedScript 写时间线：写入 + 裁剪 + 续期一次完成。
	// KEYS[1] = im:feed:{uid}
	// ARGV[1] = momentID  ARGV[2] = score
	// ARGV[3] = 保留条数  ARGV[4] = TTL(ms)
	pushFeedScript = redis.NewScript(`
redis.call('ZADD', KEYS[1], tonumber(ARGV[2]), ARGV[1])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -tonumber(ARGV[3]) - 1)
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

	// nextSeqScript 分配会话序号。键不存在时返回 0 且不落值，由调用方回源 DB 初始化。
	// KEYS[1] = im:seq:{convID}   ARGV[1] = TTL(ms)
	//
	// 为什么不能直接 INCR：seq 的唯一来源是 Redis，键一旦因重启、淘汰或误删消失，
	// INCR 会从 1 重新开始，与库里的历史 seq 重叠。客户端靠 seq 判断断档，
	// 重叠后新消息会被当成旧消息丢弃，历史分页也会取错区间。
	//
	// 所以键不存在时先返回哨兵 0，让调用方查一次 MAX(seq) 再走 initSeqScript，
	// 保证「探测」和「分配」之间不存在已经消耗掉的号段。
	//
	// INCR 与 PEXPIRE 必须同脚本：分配成功而续期丢失，这个键就永久驻留，
	// 一个会话一条、随会话数线性增长。而 seq 过期是安全事件 —— 探测到空键
	// 会回源 MAX(seq) 重建，正确性不变，只多一次走 uk_conv_seq 的查询。
	nextSeqScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
    return 0
end
local seq = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return seq
`)

	// initSeqScript 用库中最大 seq 初始化序号键，并立即分配下一个序号。
	// KEYS[1] = im:seq:{convID}  ARGV[1] = floor（库中该会话的 MAX(seq)，0 表示新会话）
	//                            ARGV[2] = TTL(ms)
	//
	// 先比后写而不是无条件 SET：并发请求会同时探测到键不存在，若各自 SET floor
	// 再 INCR，后到的那个会把前一个已分配的号覆盖掉。条件 `cur < floor` 让脚本
	// 天然幂等 —— 第一个请求设好 floor，其余请求看到的 cur 已经更大，只做 INCR。
	initSeqScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local floor = tonumber(ARGV[1])
if cur < floor then
    redis.call('SET', KEYS[1], floor)
end
local seq = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return seq
`)
)
