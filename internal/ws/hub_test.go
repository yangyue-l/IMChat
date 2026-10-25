package ws

import (
	"log/slog"
	"testing"
	"time"

	"imchat/internal/config"
)

// newTestHub 构造只用到内存结构的 Hub，不连 Redis、不起协程。
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	cfg := &config.Config{}
	cfg.Cluster.Enabled = true
	cfg.Cluster.Shards = 4
	return NewHub(cfg, nil, nil, "test-node", slog.Default())
}

// TestUnregisterOnlyCurrentConn 锁住顶号场景的下线语义。
//
// 回归背景：同设备重复登录时旧连接也要走收尾流程，若无条件清除在线态，
// 就会把新连接刚写好的记录一起删掉，直到下一次续期（最长一个续期间隔）才恢复，
// 这中间判定出来的全是假离线。分片里的 remove 有 cur != c 保护，清在线态也必须跟着它。
func TestUnregisterOnlyCurrentConn(t *testing.T) {
	h := newTestHub(t)

	old := &Client{uid: 7, device: "web", hub: h}
	cur := &Client{uid: 7, device: "web", hub: h}

	if replaced := h.register(old); replaced != nil {
		t.Fatal("首个连接不应顶掉任何人")
	}
	if replaced := h.register(cur); replaced != old {
		t.Fatalf("同设备重复登录应顶掉旧连接，实际 replaced=%v", replaced)
	}
	if n := h.connCount.Load(); n != 1 {
		t.Fatalf("顶号不增加连接数，期望 1 实际 %d", n)
	}

	if h.unregister(old) {
		t.Fatal("旧连接收尾不应移除分片中的新连接")
	}
	if n := h.connCount.Load(); n != 1 {
		t.Fatalf("旧连接收尾不应改变连接数，期望 1 实际 %d", n)
	}
	if !h.LocalOnline(7) {
		t.Fatal("顶号后新连接应仍然在线")
	}

	if !h.unregister(cur) {
		t.Fatal("当前连接应能正常注销")
	}
	if n := h.connCount.Load(); n != 0 {
		t.Fatalf("注销后期望 0 实际 %d", n)
	}
	if h.LocalOnline(7) {
		t.Fatal("注销后不应再判在线")
	}
}

// TestOnlineWithoutCluster 验证单机模式（cluster.enabled=false）的在线判定。
//
// 这里 rdb 与 online store 都是 nil：单机的 Online 必须直接读本地分片，
// 一旦走上 Redis 分支就会立刻露馅。这也是「单机先暴露在线接口」的正确姿势 ——
// 单机模式下 MarkOnline 被 cluster 开关挡住，Redis 里根本没有在线数据。
func TestOnlineWithoutCluster(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Enabled = false
	cfg.Cluster.Shards = 4
	h := NewHub(cfg, nil, nil, "test-node", slog.Default())

	if online, err := h.Online([]int64{99}); err != nil || online[99] {
		t.Fatalf("未连接的用户应判离线，实际 online=%v err=%v", online, err)
	}

	c := &Client{uid: 99, device: "web", hub: h}
	h.register(c)
	if online, err := h.Online([]int64{99}); err != nil || !online[99] {
		t.Fatalf("已连接的用户应判在线，实际 online=%v err=%v", online, err)
	}

	h.unregister(c)
	if online, err := h.Online([]int64{99}); err != nil || online[99] {
		t.Fatalf("注销后应判离线，实际 online=%v err=%v", online, err)
	}
}

// TestOnlineTTLOutlivesMissedRenew 锁住「TTL 必须覆盖一次续期失败」的约束。
//
// 回归背景：TTL 曾直接取 WSPongWait(75s)，而续期间隔是 60s。只要一次续期因
// Redis 超时失败，成员就在 t=75s 过期、下次续期在 t=120s，中间 45s 误报离线。
func TestOnlineTTLOutlivesMissedRenew(t *testing.T) {
	h := newTestHub(t)
	ping := 30 * time.Second
	h.serverCfg.WSPingInterval = config.Duration(ping)

	renew := ping * onlineRenewEvery
	if ttl := h.onlineTTL(); ttl <= renew*2 {
		t.Fatalf("TTL 必须覆盖连续两次续期间隔，ttl=%v renew=%v", ttl, renew)
	}
}
