package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"

	"imchat/internal/config"
)

// newTestCacheWith 在同一个 Redis 上换一份带 TTL 的配置，用于验证「配置真的生效」。
// newTestCache 传的是空 CacheConfig，TTL 全为 0，只能测命中逻辑、测不出过期策略。
func newTestCacheWith(t *testing.T, cfg config.CacheConfig) *Cache {
	t.Helper()
	return New(newTestCache(t).rdb, cfg)
}

// TestFriendRelationCacheAndInvalidate 验证好友/拉黑判定的缓存命中与四向失效。
//
// 这条用例钉住的是「写入用的 Key 拼法与删除用的 Key 拼法必须一致」：
// 关系缓存失效漏一个方向，表现就是「拉黑之后对方还能发消息」，而且不会报错，
// 只能靠测试暴露。同时验证 friend_ttl 配置真的传到了 Redis。
func TestFriendRelationCacheAndInvalidate(t *testing.T) {
	const friendTTL = 5 * time.Minute
	c := newTestCacheWith(t, config.CacheConfig{FriendTTL: config.Duration(friendTTL)})
	ctx := context.Background()

	base := time.Now().UnixNano()
	a, b := base, base+1
	t.Cleanup(func() {
		_ = c.DelRelations(ctx, a, b)
	})

	hits := 0
	load := func(context.Context) ([]byte, error) {
		hits++
		return []byte("1"), nil
	}

	// 两个方向各读一次：回源只应发生一次，第二次必然命中缓存
	for _, p := range [][2]int64{{a, b}, {b, a}} {
		raw, err := c.FriendRelation(ctx, p[0], p[1], load)
		if err != nil {
			t.Fatalf("读取 %d->%d 好友关系异常: %v", p[0], p[1], err)
		}
		if string(raw) != "1" {
			t.Fatalf("%d->%d 好友关系期望 \"1\"，实际 %q", p[0], p[1], raw)
		}
	}
	if hits != 2 {
		t.Fatalf("两个方向应各自回源一次，实际回源 %d 次", hits)
	}
	if _, err := c.FriendRelation(ctx, a, b, load); err != nil {
		t.Fatalf("二次读取异常: %v", err)
	}
	if hits != 2 {
		t.Fatalf("缓存未命中，回源次数期望 2，实际 %d", hits)
	}

	// TTL 必须等于配置值（TTLJitter 为 0 时不加抖动）
	key := fmt.Sprintf("%s%d:%d", keyRelFriend, a, b)
	ttl, err := c.rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取 PTTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > friendTTL {
		t.Fatalf("好友关系 Key TTL 期望落在 (0, %v]，实际 %v —— friend_ttl 没生效", friendTTL, ttl)
	}

	// 再写一条反方向的拉黑判定，然后一次失效必须清掉四条。
	// 这两次回源也走 load，所以先记下当前回源次数，后面按增量断言。
	if _, err := c.BlockedRelation(ctx, b, a, load); err != nil {
		t.Fatalf("写入反方向拉黑判定异常: %v", err)
	}
	if _, err := c.BlockedRelation(ctx, a, b, load); err != nil {
		t.Fatalf("写入正方向拉黑判定异常: %v", err)
	}
	beforeInvalidate := hits
	if err := c.DelRelations(ctx, a, b); err != nil {
		t.Fatalf("失效关系缓存异常: %v", err)
	}
	for _, p := range [][2]int64{{a, b}, {b, a}} {
		for _, prefix := range []string{keyRelFriend, keyRelBlocked} {
			k := fmt.Sprintf("%s%d:%d", prefix, p[0], p[1])
			if n, err := c.rdb.Exists(ctx, k).Result(); err != nil || n != 0 {
				t.Fatalf("%s 未被清掉（存在 %d，err=%v）", k, n, err)
			}
		}
	}

	// 失效后必须重新回源
	if _, err := c.FriendRelation(ctx, a, b, load); err != nil {
		t.Fatalf("失效后读取异常: %v", err)
	}
	if hits != beforeInvalidate+1 {
		t.Fatalf("失效后应重新回源一次，回源次数期望 %d，实际 %d", beforeInvalidate+1, hits)
	}
}

// TestGroupMemberIDsKeyAndTTL 验证群成员缓存用了 im:gmemberids: 命名，
// 且 TTL 来自 config.cache.group_member_ttl 而不是写死的 5 分钟。
func TestGroupMemberIDsKeyAndTTL(t *testing.T) {
	// 故意取 7 分钟：与历史硬编码的 5 分钟区分开，配置没生效就会露馅
	const memberTTL = 7 * time.Minute
	c := newTestCacheWith(t, config.CacheConfig{GroupMemberTTL: config.Duration(memberTTL)})
	ctx := context.Background()

	gid := time.Now().UnixNano()
	key := fmt.Sprintf("%s%d", keyGroupMemberIDs, gid)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	hits := 0
	load := func(context.Context) ([]byte, error) {
		hits++
		return []byte("[1,2,3]"), nil
	}

	raw, err := c.GroupMemberIDs(ctx, gid, load)
	if err != nil {
		t.Fatalf("读取群成员缓存异常: %v", err)
	}
	if string(raw) != "[1,2,3]" {
		t.Fatalf("群成员缓存值异常: %q", raw)
	}
	ttl, err := c.rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取 PTTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > memberTTL {
		t.Fatalf("群成员 Key TTL 期望落在 (0, %v]，实际 %v —— group_member_ttl 没生效", memberTTL, ttl)
	}

	if err := c.DelGroupMembers(ctx, gid); err != nil {
		t.Fatalf("失效群成员缓存异常: %v", err)
	}
	if n, err := c.rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("群成员 Key 未清掉（存在 %d，err=%v）", n, err)
	}
	if _, err := c.GroupMemberIDs(ctx, gid, load); err != nil {
		t.Fatalf("失效后读取异常: %v", err)
	}
	if hits != 2 {
		t.Fatalf("失效后应重新回源，回源次数期望 2，实际 %d", hits)
	}
}

// TestTokenDenyKeyUsesDigest 验证登出黑名单的 Key 后缀是摘要，不是明文令牌。
//
// 明文 JWT 200+ 字符，落进 Key 会同时放大内存并把可用凭证暴露在
// SCAN / MONITOR / 慢日志里；这条用例用「明文拼出来的 Key 必须不存在」来钉住它。
func TestTokenDenyKeyUsesDigest(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	token := "eyJhbGciOiJIUzI1NiJ9.test." + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, tokenDenyKey(token)) })

	if err := c.DenyToken(ctx, token, time.Minute); err != nil {
		t.Fatalf("写入登出黑名单异常: %v", err)
	}

	if n, err := c.rdb.Exists(ctx, keyTokenDeny+token).Result(); err != nil || n != 0 {
		t.Fatalf("明文令牌 Key 不该存在（存在 %d，err=%v）", n, err)
	}

	sum := sha256.Sum256([]byte(token))
	digestKey := keyTokenDeny + hex.EncodeToString(sum[:])
	if n, err := c.rdb.Exists(ctx, digestKey).Result(); err != nil || n != 1 {
		t.Fatalf("摘要 Key 应存在（存在 %d，err=%v）", n, err)
	}

	denied, err := c.IsTokenDenied(ctx, token)
	if err != nil {
		t.Fatalf("查询登出状态异常: %v", err)
	}
	if !denied {
		t.Fatal("已登出的令牌必须判定为 denied")
	}

	if ttl, err := c.rdb.PTTL(ctx, digestKey).Result(); err != nil || ttl <= 0 {
		t.Fatalf("摘要 Key 应带 TTL（ttl=%v，err=%v）", ttl, err)
	}
}
