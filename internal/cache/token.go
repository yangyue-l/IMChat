package cache

import (
	"context"
	"time"
)

// 登出令牌黑名单。TTL 与令牌剩余有效期对齐，过期即自然回收，
// 不需要额外的清理任务。

// DenyToken 把登出的令牌加入黑名单，TTL 与令牌剩余有效期一致即可。
func (c *Cache) DenyToken(ctx context.Context, token string, ttl time.Duration) error {
	return c.rdb.Set(ctx, keyTokenDeny+token, 1, ttl).Err()
}

// IsTokenDenied 检查令牌是否已登出。
func (c *Cache) IsTokenDenied(ctx context.Context, token string) (bool, error) {
	n, err := c.rdb.Exists(ctx, keyTokenDeny+token).Result()
	return n > 0, err
}
