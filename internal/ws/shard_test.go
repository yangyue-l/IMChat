package ws

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// 这些用例自建 shard 而不是走 Hub.shardOf：分片只是 uid 的散列落点，
// 与下面这些语义无关；走 Hub 还得故意挑落在同一片的 uid，测试会比被测代码更难读。
// Hub 层的计数（connCount）与注册入口另有 TestUnregisterOnlyCurrentConn 覆盖。

// TestAcceptSameDeviceNeverHitsCap 锁住「同设备重连不受 maxConns 限制」。
//
// 回归背景：accept 的上限判断是 len(m) < maxConns。如果顺序反过来 ——
// 先判额度再看是不是同设备 —— 单设备用户在自己的额度用满时重连会被 429
// 拦在 Upgrade 之前，表现就是「网络一抖就登不上」，而分片里明明只有他自己。
func TestAcceptSameDeviceNeverHitsCap(t *testing.T) {
	s := newShard()
	s.add(&Client{uid: 1, device: "web"})

	if !s.accept(1, "web", 1) {
		t.Fatal("同设备重连在额度用满时必须放行：它只会顶掉旧连接，不增加总数")
	}
	if s.accept(1, "ios", 1) {
		t.Fatal("新设备在额度用满时不应放行")
	}
	if !s.accept(1, "ios", 2) {
		t.Fatal("额度未满时新设备应放行")
	}
	if !s.accept(2, "web", 1) {
		t.Fatal("没连过的用户不该受别人的额度影响")
	}
}

// TestReplaceKeepsSingleConnPerDevice 锁住顶号后的两个不变量：
// add 要把被顶掉的旧连接交回去（调用方靠它给旧连接发 kick 帧），
// 并且分片里同设备只留一条记录。
//
// 与 TestUnregisterOnlyCurrentConn 不重复：那条用例看的是 connCount 与
// unregister 的联动，这里看的是分片内部的 map 内容与连接总数。
func TestReplaceKeepsSingleConnPerDevice(t *testing.T) {
	s := newShard()
	web1 := &Client{uid: 1, device: "web"}
	ios := &Client{uid: 1, device: "ios"}

	if old := s.add(web1); old != nil {
		t.Fatalf("首次接入不应顶掉任何连接，实际 %v", old)
	}
	if old := s.add(ios); old != nil {
		t.Fatal("不同设备共存，不该互相顶掉")
	}

	web2 := &Client{uid: 1, device: "web"}
	if old := s.add(web2); old != web1 {
		t.Fatalf("同设备再登录应返回旧连接，实际 %v", old)
	}
	if n := s.count(); n != 2 {
		t.Fatalf("顶号不增加连接数，期望 2（web+ios）实际 %d", n)
	}
	if got := len(s.conns[1]); got != 2 {
		t.Fatalf("该用户应只留 2 条设备记录，实际 %d", got)
	}
}

// TestRemoveRejectsStaleConn 锁住「旧连接晚到的收尾不能删掉顶号后的新连接」。
//
// 这里直接测分片：Hub.unregister 只返回 bool，看不到 map 里剩下的到底是哪一条，
// 而误删的后果是用户被判离线直到下一次心跳续期。
func TestRemoveRejectsStaleConn(t *testing.T) {
	s := newShard()
	old := &Client{uid: 5, device: "web"}
	cur := &Client{uid: 5, device: "web"}
	s.add(old)
	s.add(cur)

	if s.remove(old) {
		t.Fatal("旧连接的收尾不应移除已顶替它的新连接")
	}
	if !s.has(5) {
		t.Fatal("顶号后新连接应仍在线")
	}
	if !s.remove(cur) {
		t.Fatal("当前连接应能正常移除")
	}
	if s.has(5) {
		t.Fatal("移除后不应再判在线")
	}
}

// TestRemoveReclaimsUidEntry 锁住「最后一条设备下线后清掉 uid 那一层 map」。
//
// 只删 device 不删 uid 时 count 看起来是对的，但 conns 会随「登录过的用户数」
// 单调增长 —— 一个只涨不落的 map，在长跑进程里就是内存泄漏。
func TestRemoveReclaimsUidEntry(t *testing.T) {
	s := newShard()
	a := &Client{uid: 9, device: "web"}
	b := &Client{uid: 9, device: "ios"}
	s.add(a)
	s.add(b)

	if !s.remove(a) {
		t.Fatal("当前登记的连接应能移除")
	}
	if !s.has(9) {
		t.Fatal("还有一条设备在线时不应判该用户离线")
	}
	if !s.remove(b) {
		t.Fatal("最后一条连接应能移除")
	}
	if n := s.count(); n != 0 {
		t.Fatalf("全部下线后连接数应为 0，实际 %d", n)
	}
	if _, exist := s.conns[9]; exist {
		t.Fatal("最后一条设备下线后应清掉 uid 条目，否则这张 map 只涨不落")
	}
}

// TestCanAcceptWithoutLimit 锁住「maxConns <= 0 表示不限制」。
//
// 配置里的 0 会被 validate 归一成 5，但 Hub 也会被脚本与测试直接构造；
// 那种场景下若按 len(m) < 0 判定，会把所有新连接拒掉。
func TestCanAcceptWithoutLimit(t *testing.T) {
	h := newTestHub(t)
	for d := 0; d < 10; d++ {
		h.register(newTestClient(h, 3, fmt.Sprintf("d%d", d)))
	}

	if !h.CanAccept(3, "d99", 0) {
		t.Fatal("maxConns=0 应表示不限制")
	}
	if !h.CanAccept(3, "d99", -1) {
		t.Fatal("maxConns<0 应表示不限制")
	}
	if h.CanAccept(3, "d99", 10) {
		t.Fatal("已达额度 10 时新设备应被拒")
	}
}

// TestConcurrentAddRemoveKeepsCountsExact 用并发压分片锁的临界区。
//
// 锁漏加会直接崩在 "concurrent map writes"；锁加了但语义错（比如顶号时多算一次）
// 则只有计数类断言能拦住 —— connCount 正是 /metrics 与在线人数的口径，
// 一旦与真实连接数偏离就再也对不回去。
//
// 本地 Windows 不能跑 -race（无 cgo，见计划 D14 记录），并发正确性在 CI 的
// go test -race 里复核；这里的断言保证不上锁时也能发现问题。
func TestConcurrentAddRemoveKeepsCountsExact(t *testing.T) {
	h := newTestHub(t)

	const users = 8
	const devicesPerUser = 4

	clients := make([]*Client, 0, users*devicesPerUser)
	for uid := int64(1); uid <= users; uid++ {
		for d := 0; d < devicesPerUser; d++ {
			clients = append(clients, newTestClient(h, uid, fmt.Sprintf("d%d", d)))
		}
	}

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			h.register(c)
		}(c)
	}
	wg.Wait()

	want := int64(len(clients))
	if got := h.connCount.Load(); got != want {
		t.Fatalf("并发注册后连接数应为 %d，实际 %d", want, got)
	}
	if got := shardConnTotal(h); got != int(want) {
		t.Fatalf("connCount 应与分片实际连接数一致，期望 %d 实际 %d", want, got)
	}

	var removed atomic.Int64
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			if h.unregister(c) {
				removed.Add(1)
			}
		}(c)
	}
	wg.Wait()

	if got := removed.Load(); got != want {
		t.Fatalf("每条已登记连接都应恰好移除一次，期望 %d 实际 %d", want, got)
	}
	if got := h.connCount.Load(); got != 0 {
		t.Fatalf("全部下线后计数应归零，实际 %d", got)
	}
	if got := shardConnTotal(h); got != 0 {
		t.Fatalf("全部下线后分片应为空，实际 %d 条残留", got)
	}
	assertNoUidEntryLeft(t, h)
}

// TestConcurrentSameDeviceLoginLeavesOne 锁住「并发抢同一设备只留一条连接、计数只加一次」。
//
// 同一账号在不同入口几乎同时重连是常态（重连风暴、多标签页）。若 add 与计数
// 不是同一个临界区，会出现「16 次登录、connCount=16、实际只有 1 条连接」的
// 长期偏差，而且 unregister 会把计数减成负数。
func TestConcurrentSameDeviceLoginLeavesOne(t *testing.T) {
	h := newTestHub(t)

	const conns = 16
	clients := make([]*Client, conns)
	for i := range clients {
		clients[i] = newTestClient(h, 42, "web")
	}

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			h.register(c)
		}(c)
	}
	wg.Wait()

	if got := h.connCount.Load(); got != 1 {
		t.Fatalf("同设备并发登录不应累加连接数，期望 1 实际 %d", got)
	}
	if got := h.shardOf(42).count(); got != 1 {
		t.Fatalf("分片里同设备应只留一条，实际 %d", got)
	}

	var removed atomic.Int64
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			if h.unregister(c) {
				removed.Add(1)
			}
		}(c)
	}
	wg.Wait()

	// 只有最终登记的那一条能移除；其余 15 条的收尾都该被 remove 的 cur != c 挡住
	if got := removed.Load(); got != 1 {
		t.Fatalf("同设备多条连接的收尾只应有一条生效，实际 %d", got)
	}
	if got := h.connCount.Load(); got != 0 {
		t.Fatalf("计数应收敛到 0，实际 %d", got)
	}
	assertNoUidEntryLeft(t, h)
}

// shardConnTotal 汇总所有分片的实际连接数，用来与 connCount 对账。
func shardConnTotal(h *Hub) int {
	n := 0
	for _, s := range h.shards {
		n += s.count()
	}
	return n
}

// assertNoUidEntryLeft 校验每个分片都回收掉了空的 uid 条目。
func assertNoUidEntryLeft(t *testing.T, h *Hub) {
	t.Helper()
	for i, s := range h.shards {
		s.mu.RLock()
		left := len(s.conns)
		s.mu.RUnlock()
		if left != 0 {
			t.Fatalf("分片 %d 还残留 %d 个 uid 条目，空 map 应在最后一条设备下线时一并删除", i, left)
		}
	}
}

// TestShardDevices 验证设备名列举：按名字排序、不串号、注销后立刻消失。
//
// 排序不是锦上添花：map 遍历顺序随机，原样输出会让「登录设备管理」每次刷新
// 都换一个行序，用户会以为设备列表在乱跳。conn 则要锁住「只取本设备」，
// 取错就等于踢错了端。
func TestShardDevices(t *testing.T) {
	s := newShard()
	if got := s.devices(1); got != nil {
		t.Fatalf("没连过的用户应返回 nil，实际 %v", got)
	}

	web := &Client{uid: 1, device: "web"}
	s.add(web)
	s.add(&Client{uid: 1, device: "android"})
	s.add(&Client{uid: 2, device: "pad"})

	got := s.devices(1)
	if len(got) != 2 || got[0] != "android" || got[1] != "web" {
		t.Fatalf("期望按名字排序的 [android web]，实际 %v", got)
	}
	if c := s.conn(1, "web"); c != web {
		t.Fatalf("conn(1, web) 应取到 web 连接，实际 %v", c)
	}
	if c := s.conn(1, "pad"); c != nil {
		t.Fatalf("conn 不应串到别的用户，实际 %v", c)
	}

	s.remove(web)
	if got := s.devices(1); len(got) != 1 || got[0] != "android" {
		t.Fatalf("注销后应只剩 android，实际 %v", got)
	}
}
