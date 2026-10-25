package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// 登出令牌黑名单。TTL 与令牌剩余有效期对齐，过期即自然回收，
// 不需要额外的清理任务。
//
// Key 后缀存 token 的 SHA-256 摘要，不存 token 本身：JWT 有 200+ 字符，
// 明文拼进 Key 会让每条记录的内存开销翻几倍，而且会原样出现在 SCAN、
// MONITOR、慢日志和监控面板里 —— 等于把一份还能用的凭证抄到最容易泄露的地方。
// 摘要不损失唯一性：校验时用同一个 token 算摘要，能命中就说明已登出。

// DenyToken 把登出的令牌加入黑名单，TTL 与令牌剩余有效期一致即可。
func (c *Cache) DenyToken(ctx context.Context, token string, ttl time.Duration) error {
	return c.rdb.Set(ctx, tokenDenyKey(token), 1, ttl).Err()
}

// IsTokenDenied 检查令牌是否已登出。
func (c *Cache) IsTokenDenied(ctx context.Context, token string) (bool, error) {
	n, err := c.rdb.Exists(ctx, tokenDenyKey(token)).Result()
	return n > 0, err
}

// tokenDenyKey 用摘要代替明文 token 拼 Key。
func tokenDenyKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return keyTokenDeny + hex.EncodeToString(sum[:])
}
