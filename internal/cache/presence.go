package cache

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 设备在线状态。多端共用一个 Key，用 zset 的 score 存「设备自身」的过期时刻，
// 而不是靠 key 级 TTL —— 否则任一设备心跳都会刷新整个 key 的 TTL，
// 已断开的设备成员会被无限期保留，在线判定恒为 true。

// SetOnline 记录设备在线。zset 的 score 是该设备自身的过期时刻，
// 每次写入顺手清理已过期成员，因此单设备掉线不会被其他设备的心跳「续命」。
func (c *Cache) SetOnline(ctx context.Context, uid int64, device string, ttl time.Duration) error {
	key := fmt.Sprintf("%s%d", keyOnline, uid)
	now := time.Now().UnixMilli()
	args := []any{device, now + ttl.Milliseconds(), now, ttl.Milliseconds()}
	err := setOnlineScript.Run(ctx, c.rdb, []string{key}, args...).Err()
	if err != nil && strings.Contains(err.Error(), "WRONGTYPE") {
		// 兼容旧版本把该 key 写成 set 的情况，清掉重建
		_ = c.rdb.Del(ctx, key).Err()
		err = setOnlineScript.Run(ctx, c.rdb, []string{key}, args...).Err()
	}
	return err
}

// SetOffline 移除某个在线设备。
func (c *Cache) SetOffline(ctx context.Context, uid int64, device string) error {
	return c.rdb.ZRem(ctx, fmt.Sprintf("%s%d", keyOnline, uid), device).Err()
}

// OnlineSet 返回这批用户里在线的那些，map 中存在的 uid 即为在线。
//
// 一个用户一个 Key，逐个 ZCOUNT 就是 N 次往返，而调用方（好友列表、群消息扇出）
// 一次要问几十到几百人。这里用 pipeline 把 N 个 ZCOUNT 压成一次往返。
// 只收「在线」的结论，不在 map 里就是离线，省掉一半没有信息量的 false。
//
// 返回 error 时调用方应视为「状态未知」：查不到不等于不在线，别据此判定离线。
func (c *Cache) OnlineSet(ctx context.Context, uids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.IntCmd, 0, len(uids))
	for _, uid := range uids {
		cmds = append(cmds, pipe.ZCount(ctx, fmt.Sprintf("%s%d", keyOnline, uid), now, "+inf"))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, cmd := range cmds {
		if n, err := cmd.Result(); err == nil && n > 0 {
			out[uids[i]] = true
		}
	}
	return out, nil
}
