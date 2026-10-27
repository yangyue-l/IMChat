package ws

import (
	"context"
	"errors"
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

// stubOnlineStore 记录回源请求，用来验证「谁被问了 Redis」。
type stubOnlineStore struct {
	online map[int64]bool
	err    error
	asked  [][]int64
}

func (s *stubOnlineStore) SetOnline(context.Context, int64, string, time.Duration) error {
	return nil
}

func (s *stubOnlineStore) SetOffline(context.Context, int64, string) error { return nil }

func (s *stubOnlineStore) OnlineSet(_ context.Context, uids []int64) (map[int64]bool, error) {
	s.asked = append(s.asked, uids)
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[int64]bool, len(uids))
	for _, uid := range uids {
		if s.online[uid] {
			out[uid] = true
		}
	}
	return out, nil
}

// TestOnlineMergesLocalAndRemote 验证集群模式的合并语义：
// 本地已连上的人不重复问 Redis，未命中的才批量回源，最终结果两边取并集。
func TestOnlineMergesLocalAndRemote(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Enabled = true
	cfg.Cluster.Shards = 4
	store := &stubOnlineStore{online: map[int64]bool{2: true}}
	h := NewHub(cfg, nil, store, "test-node", slog.Default())

	local := &Client{uid: 1, device: "web", hub: h}
	h.register(local)

	got, err := h.Online([]int64{1, 2, 3})
	if err != nil {
		t.Fatalf("Online 异常: %v", err)
	}
	if !got[1] {
		t.Fatal("本地已连接的用户应判在线")
	}
	if !got[2] {
		t.Fatal("远端在线的用户应判在线")
	}
	if got[3] {
		t.Fatal("谁都没连的用户不应判在线")
	}

	// 1 已经在本地命中，回源请求里不该出现它
	if len(store.asked) != 1 {
		t.Fatalf("应恰好回源一次，实际 %d 次", len(store.asked))
	}
	if want := []int64{2, 3}; len(store.asked[0]) != 2 || store.asked[0][0] != want[0] || store.asked[0][1] != want[1] {
		t.Fatalf("回源名单应只剩未本地命中的 [2 3]，实际 %v", store.asked[0])
	}
}

// TestOnlinePropagatesStoreError 验证回源失败不会被吞成「全部离线」。
func TestOnlinePropagatesStoreError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Enabled = true
	cfg.Cluster.Shards = 4
	boom := errors.New("redis down")
	h := NewHub(cfg, nil, &stubOnlineStore{err: boom}, "test-node", slog.Default())

	if _, err := h.Online([]int64{2}); !errors.Is(err, boom) {
		t.Fatalf("应透出回源错误，实际 %v", err)
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

// newTestClient 构造不带真实 websocket.Conn 的连接，只够跑推送路径。
func newTestClient(h *Hub, uid int64, device string) *Client {
	return &Client{
		uid:    uid,
		device: device,
		hub:    h,
		send:   make(chan []byte, 4),
		closed: make(chan struct{}),
		log:    h.log,
	}
}

// TestPushToUsersCoversEveryConn 验证批量推送在一次加锁下覆盖全部接收者。
//
// 回归背景：shard.push 改成收一批 uid 之后，一旦分组或内层遍历写错
// （只用第一个 uid、或漏掉同用户的第二个设备），消息会静默只送到一部分人 ——
// 这种 bug 不报错、不 panic，只是有人收不到消息，只有测试能拦住。
func TestPushToUsersCoversEveryConn(t *testing.T) {
	h := newTestHub(t)

	clients := []*Client{
		newTestClient(h, 1, "web"),
		newTestClient(h, 1, "ios"),
		newTestClient(h, 2, "web"),
		newTestClient(h, 3, "web"),
	}
	for _, c := range clients {
		h.register(c)
	}

	payload := []byte(`{"type":"message"}`)
	if n := h.PushToUsers([]int64{1, 2, 3}, payload); n != len(clients) {
		t.Fatalf("应投递到全部 %d 条连接，实际 %d", len(clients), n)
	}
	for _, c := range clients {
		select {
		case got := <-c.send:
			if string(got) != string(payload) {
				t.Fatalf("uid=%d device=%s 载荷不对: %s", c.uid, c.device, got)
			}
		default:
			t.Fatalf("uid=%d device=%s 没收到推送", c.uid, c.device)
		}
	}

	// 混合离线 uid 时，投递数只算真实连接
	if n := h.PushToUsers([]int64{1, 999}, payload); n != 2 {
		t.Fatalf("离线用户不计入投递数，期望 2 实际 %d", n)
	}
}
