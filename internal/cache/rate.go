package cache

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 滑动窗口限流。Key 为 zset，成员是每次放行的记录，score 是放行时刻；
// 判定时先删掉滑出窗口的记录再统计，因此不存在固定窗口那种
// 「窗口边界漏出双倍额度」的突刺。脚本见 lua.go: slideWindowScript。

// RateState 是一次滑动窗口限流的判定结果。
type RateState struct {
	Used       int64         // 当前窗口内已放行的请求数
	Allowed    bool          // 本次是否放行
	RetryAfter time.Duration // 被拒时建议的重试等待，等于最早一次放行滑出窗口的时间
}

// rateSeq 给滑动窗口的 zset 成员补唯一后缀。
// score 只记录秒/毫秒级时间戳，同一毫秒内的并发请求会拿到相同 score，
// 若成员也相同则 ZADD 会覆盖而不是累加，窗口计数就会少算。
var rateSeq atomic.Uint64

// AllowSlide 滑动窗口分布式限流，跨实例共享同一份配额。
//
// 窗口内的放行记录存在 zset 里，score 是放行时刻；每次判定先删掉滑出窗口的
// 记录再统计，因此不存在固定窗口那种「窗口边界双倍额度」的突刺。
func (c *Cache) AllowSlide(ctx context.Context, key string, limit int64, window time.Duration) (RateState, error) {
	if limit <= 0 || window <= 0 {
		return RateState{Allowed: true}, nil
	}

	now := time.Now().UnixMilli()
	member := strconv.FormatInt(now, 10) + ":" + strconv.FormatUint(rateSeq.Add(1), 10)
	fullKey := keyRate + key
	args := []any{now, window.Milliseconds(), member, limit}

	res, err := slideWindowScript.Run(ctx, c.rdb, []string{fullKey}, args...).Slice()
	if err != nil && strings.Contains(err.Error(), "WRONGTYPE") {
		// 旧版本把限流键写成 string，升级后同名键类型不符；清掉重建即可，
		// 最多丢掉一个窗口的计数，比让接口整体报错划算。
		_ = c.rdb.Del(ctx, fullKey).Err()
		res, err = slideWindowScript.Run(ctx, c.rdb, []string{fullKey}, args...).Slice()
	}
	if err != nil {
		return RateState{Allowed: true}, err // 限流组件异常时放行，由调用方的令牌桶兜底
	}
	if len(res) < 3 {
		return RateState{Allowed: true}, nil
	}

	used, _ := res[0].(int64)
	retry, _ := res[1].(int64)
	allowed, _ := res[2].(int64)
	return RateState{
		Used:       used,
		Allowed:    allowed == 1,
		RetryAfter: time.Duration(retry) * time.Millisecond,
	}, nil
}
