package cache

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 设备在线状态。多端共用一个 Key，用 zset 的 score 存「设备自身」的过期时刻，
// 而不是靠 key 级 TTL —— 否则任一设备心跳都会刷新整个 key 的 TTL，
// 已断开的设备成员会被无限期保留，IsOnline 恒为 true。

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

// IsOnline 判断用户是否有任意设备在线。按时间戳过滤，不依赖 key 级 TTL。
func (c *Cache) IsOnline(ctx context.Context, uid int64) (bool, error) {
	key := fmt.Sprintf("%s%d", keyOnline, uid)
	n, err := c.rdb.ZCount(ctx, key, fmt.Sprintf("%d", time.Now().UnixMilli()), "+inf").Result()
	return n > 0, err
}
