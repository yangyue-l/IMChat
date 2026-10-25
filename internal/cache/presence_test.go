package cache

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestPresenceKeyType 锁住在线的 Key 类型必须是 zset。
//
// 回归背景：WS 侧曾用 SAdd/SRem 写 im:online:{uid}，而握手用 zset 写同一个 Key，
// 两种类型混用后所有续期与下线都在 WRONGTYPE 上静默失败，在线态过期即永久离线。
func TestPresenceKeyType(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	uid := time.Now().UnixNano()
	key := keyOnline + strconv.FormatInt(uid, 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	if err := c.SetOnline(ctx, uid, "web", time.Minute); err != nil {
		t.Fatalf("SetOnline 异常: %v", err)
	}
	if typ, err := c.rdb.Type(ctx, key).Result(); err != nil || typ != "zset" {
		t.Fatalf("在线状态期望 zset，实际 %q err=%v", typ, err)
	}
	// 任何试图用 set 命令写同一 Key 的实现都会在这里暴露
	if err := c.rdb.SAdd(ctx, key, "web").Err(); err == nil {
		t.Fatal("同一 Key 不应接受 set 写入，否则在线状态出现类型冲突")
	}

	if err := c.SetOffline(ctx, uid, "web"); err != nil {
		t.Fatalf("SetOffline 异常: %v", err)
	}
	if online, err := c.OnlineSet(ctx, []int64{uid}); err != nil || online[uid] {
		t.Fatalf("下线后期望不在线，实际 online=%v err=%v", online, err)
	}
}

// TestPresenceExpirePerDevice 验证过期按设备独立计算：
// 一台设备心跳不能给已过期的设备续命，否则在线判定会一直为真。
func TestPresenceExpirePerDevice(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	uid := time.Now().UnixNano()
	key := keyOnline + strconv.FormatInt(uid, 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	// 负 TTL 表示该设备已过期，脚本写入时应顺手清理掉它
	if err := c.SetOnline(ctx, uid, "stale", -time.Second); err != nil {
		t.Fatalf("SetOnline(stale) 异常: %v", err)
	}
	if err := c.SetOnline(ctx, uid, "live", time.Minute); err != nil {
		t.Fatalf("SetOnline(live) 异常: %v", err)
	}

	members, err := c.rdb.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		t.Fatalf("读取在线集合异常: %v", err)
	}
	if len(members) != 1 || members[0] != "live" {
		t.Fatalf("期望只保留 live，实际 %v", members)
	}
	if online, err := c.OnlineSet(ctx, []int64{uid}); err != nil || !online[uid] {
		t.Fatalf("期望在线，实际 online=%v err=%v", online, err)
	}
}

// TestPresenceOnlineSet 验证批量在线判定：只把真正在线的 uid 放进结果，
// 且与单查一致地按 score 过滤过期设备（好友列表与离线补偿共用这条路径）。
func TestPresenceOnlineSet(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	base := time.Now().UnixNano()
	uids := []int64{base, base + 1, base + 2}
	t.Cleanup(func() {
		for _, uid := range uids {
			_ = c.rdb.Del(ctx, keyOnline+strconv.FormatInt(uid, 10))
		}
	})

	if got, err := c.OnlineSet(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("空列表期望空结果，实际 %v err=%v", got, err)
	}
	if got, err := c.OnlineSet(ctx, uids); err != nil || len(got) != 0 {
		t.Fatalf("全部离线期望空结果，实际 %v err=%v", got, err)
	}

	if err := c.SetOnline(ctx, uids[2], "web", time.Minute); err != nil {
		t.Fatalf("SetOnline 异常: %v", err)
	}
	got, err := c.OnlineSet(ctx, uids)
	if err != nil {
		t.Fatalf("OnlineSet 异常: %v", err)
	}
	if len(got) != 1 || !got[uids[2]] {
		t.Fatalf("期望只返回 uids[2]，实际 %v", got)
	}

	// 负 TTL 写入即过期，脚本会顺手清掉成员，此时不应再算在线
	if err := c.SetOnline(ctx, uids[2], "web", -time.Second); err != nil {
		t.Fatalf("SetOnline(过期) 异常: %v", err)
	}
	if got, err := c.OnlineSet(ctx, uids); err != nil || len(got) != 0 {
		t.Fatalf("过期后期望空结果，实际 %v err=%v", got, err)
	}
}
