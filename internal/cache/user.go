package cache

import (
	"context"
	"fmt"
	"time"
)

// 用户资料与登录失败计数。
// 用户资料是「读多写少 + 资料变更即失效」的标准读穿缓存，TTL 见 config.cache。

// LoadUser 带数据源的读取，供 service 层注入回源逻辑。
func (c *Cache) LoadUser(ctx context.Context, uid int64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return c.Remember(ctx, fmt.Sprintf("%s%d", keyUser, uid), c.cfg.UserTTL.Get(), load)
}

// DelUser 失效用户资料缓存。
func (c *Cache) DelUser(ctx context.Context, uids ...int64) error {
	if len(uids) == 0 {
		return nil
	}
	keys := make([]string, 0, len(uids))
	for _, uid := range uids {
		keys = append(keys, fmt.Sprintf("%s%d", keyUser, uid))
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// RecordLoginFailure 记录一次登录失败并返回累计次数。
// 计数与 TTL 用脚本原子完成：EXPIRE 一旦丢失，计数停在阈值上永不过期，
// 而读路径超阈值就直接拒绝，账号会被永久锁死。
func (c *Cache) RecordLoginFailure(ctx context.Context, username string, window time.Duration) (int64, error) {
	return incrFailScript.Run(ctx, c.rdb, []string{keyLoginFail + username}, window.Milliseconds()).Int64()
}

// LoginFailures 读取登录失败次数。key 不存在或异常时返回 0，不阻断登录。
func (c *Cache) LoginFailures(ctx context.Context, username string) int64 {
	n, err := c.rdb.Get(ctx, keyLoginFail+username).Int64()
	if err != nil {
		return 0
	}
	return n
}

// ResetLoginFailures 登录成功后清零失败计数。
func (c *Cache) ResetLoginFailures(ctx context.Context, username string) error {
	return c.rdb.Del(ctx, keyLoginFail+username).Err()
}
