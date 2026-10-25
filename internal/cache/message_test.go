package cache

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// allocate 复刻 service 侧的序号分配流程：探测到键不存在时先回源再初始化。
func allocate(ctx context.Context, c *Cache, convID string, floor int64) (int64, error) {
	seq, err := c.NextSeq(ctx, convID)
	if err != nil || seq != 0 {
		return seq, err
	}
	return c.InitSeq(ctx, convID, floor)
}

// TestSeqNormalPath 验证首次分配从 1 开始、后续严格递增。
func TestSeqNormalPath(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	convID := "test:seq:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, keySeq+convID) })

	for want := int64(1); want <= 3; want++ {
		got, err := allocate(ctx, c, convID, 0)
		if err != nil {
			t.Fatalf("第 %d 次分配异常: %v", want, err)
		}
		if got != want {
			t.Fatalf("第 %d 次分配期望 %d，实际 %d", want, want, got)
		}
	}
}

// TestSeqRebuildAfterKeyLost 验证 Redis 键丢失后序号从库中最大 seq 之后继续，
// 而不是从 1 重来与历史重叠。
func TestSeqRebuildAfterKeyLost(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	convID := "test:seq:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := keySeq + convID
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	const history = 100
	got, err := allocate(ctx, c, convID, history)
	if err != nil {
		t.Fatalf("初始化分配异常: %v", err)
	}
	if got != history+1 {
		t.Fatalf("初始化后期望 %d，实际 %d", history+1, got)
	}

	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("模拟丢键失败: %v", err)
	}
	probe, err := c.NextSeq(ctx, convID)
	if err != nil {
		t.Fatalf("丢键后探测异常: %v", err)
	}
	if probe != 0 {
		t.Fatalf("丢键后应返回哨兵 0，实际 %d", probe)
	}

	got, err = allocate(ctx, c, convID, history)
	if err != nil {
		t.Fatalf("丢键后恢复异常: %v", err)
	}
	if got <= history {
		t.Fatalf("恢复后序号必须大于历史最大值 %d，实际 %d", history, got)
	}
}

// TestSeqKeyHasTTL 验证序号键带滑动 TTL：分配后必有 TTL，且每次分配都续期。
//
// 这条用例钉住的是「seq 键不能永久驻留」这个约束：无 TTL 时每个会话一条 string
// 常驻内存、随会话数线性增长，且没有任何机制回收。过期本身是安全的 ——
// 探测到空键会回源 MAX(seq) 重建，所以续期必须和分配在同一次脚本里完成。
func TestSeqKeyHasTTL(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	convID := "test:seq:ttl:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := keySeq + convID
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	if _, err := allocate(ctx, c, convID, 0); err != nil {
		t.Fatalf("首次分配异常: %v", err)
	}
	ttl, err := c.rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取 PTTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > seqTTL {
		t.Fatalf("序号键 TTL 异常，期望落在 (0, %v]，实际 %v", seqTTL, ttl)
	}

	// 把 TTL 压到 1s 后再分配一次，必须续期回接近 seqTTL，否则就是不续期的死键。
	// 这里不能压到 1ms：键在 NextSeq 执行前就已消失时，脚本会走「返回哨兵 0、
	// 由调用方回源」的分支（见 lua.go nextSeqScript），根本没机会续期，测试会假红。
	if err := c.rdb.PExpire(ctx, key, time.Second).Err(); err != nil {
		t.Fatalf("压缩 TTL 失败: %v", err)
	}
	if _, err := c.NextSeq(ctx, convID); err != nil {
		t.Fatalf("续期分配异常: %v", err)
	}
	renewed, err := c.rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取续期后 PTTL 失败: %v", err)
	}
	if renewed < seqTTL/2 {
		t.Fatalf("序号键未续期，期望接近 %v，实际 %v", seqTTL, renewed)
	}
}

// TestReadKeyHasTTL 验证已读位置 hash 带 TTL：MarkRead 与 SetReadSeqIfAbsent 都会续期。
//
// 已读 hash 是一个用户一条、field 数等于他的会话数，无 TTL 时同样是永久驻留。
// 过期同样无损：conversation_reads 表是回源兜底，缺失时按群最新 seq 对齐。
func TestReadKeyHasTTL(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	uid := time.Now().UnixNano()
	convID := "test:read:ttl:" + strconv.FormatInt(uid, 10)
	key := fmt.Sprintf("%s%d", keyRead, uid)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	assertTTL := func(stage string) {
		t.Helper()
		ttl, err := c.rdb.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("%s 读取 PTTL 失败: %v", stage, err)
		}
		if ttl <= 0 || ttl > readTTL {
			t.Fatalf("%s 已读 hash TTL 异常，期望落在 (0, %v]，实际 %v", stage, readTTL, ttl)
		}
	}

	// 入群对齐路径：HSETNX 也要带续期，否则只对齐不上报的会话永远没有 TTL
	if err := c.SetReadSeqIfAbsent(ctx, uid, convID, 10); err != nil {
		t.Fatalf("写入已读位置失败: %v", err)
	}
	assertTTL("SetReadSeqIfAbsent 后")

	// 上报已读路径
	if _, err := c.MarkRead(ctx, uid, convID, 20); err != nil {
		t.Fatalf("上报已读失败: %v", err)
	}
	assertTTL("MarkRead 后")
}

// TestUnreadKeyHasTTL 验证未读计数「自增」与「续期」在同一次脚本里完成。
//
// 未读 hash 是一个用户一条、field 数等于他的会话数，一旦 HINCRBY 生效而续期
// 丢失，这个键就永久驻留且没有任何回收路径 —— 它不像 seq 有回源重建的分支。
// 所以这条用例钉住的是「计数与 TTL 同生」，而不是「计数正确」本身。
func TestUnreadKeyHasTTL(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	base := time.Now().UnixNano()
	uids := []int64{base, base + 1}
	convID := "test:unread:ttl"

	keys := make([]string, 0, len(uids))
	for _, uid := range uids {
		keys = append(keys, fmt.Sprintf("%s%d", keyUnread, uid))
	}
	t.Cleanup(func() { _ = c.rdb.Del(ctx, keys...) })

	// 一次调用要覆盖全部接收者：群聊场景下漏掉一个 Key 就是一个人看不到红点
	if err := c.IncrUnread(ctx, convID, uids, 1); err != nil {
		t.Fatalf("首次自增异常: %v", err)
	}
	for _, key := range keys {
		got, err := c.rdb.HGet(ctx, key, convID).Int64()
		if err != nil {
			t.Fatalf("%s 读取未读数失败: %v", key, err)
		}
		if got != 1 {
			t.Fatalf("%s 未读数期望 1，实际 %d", key, got)
		}
		ttl, err := c.rdb.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("%s 读取 PTTL 失败: %v", key, err)
		}
		if ttl <= 0 || ttl > unreadTTL {
			t.Fatalf("%s 未读 hash TTL 异常，期望落在 (0, %v]，实际 %v", key, unreadTTL, ttl)
		}
		// 把 TTL 压到 1s，下一次自增必须把它续回接近 unreadTTL
		if err := c.rdb.PExpire(ctx, key, time.Second).Err(); err != nil {
			t.Fatalf("%s 压缩 TTL 失败: %v", key, err)
		}
	}

	if err := c.IncrUnread(ctx, convID, uids, 1); err != nil {
		t.Fatalf("二次自增异常: %v", err)
	}
	for _, key := range keys {
		got, err := c.rdb.HGet(ctx, key, convID).Int64()
		if err != nil {
			t.Fatalf("%s 读取未读数失败: %v", key, err)
		}
		if got != 2 {
			t.Fatalf("%s 未读数期望累加到 2，实际 %d", key, got)
		}
		renewed, err := c.rdb.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("%s 读取续期后 PTTL 失败: %v", key, err)
		}
		if renewed < unreadTTL/2 {
			t.Fatalf("%s 未读 hash 未续期，期望接近 %v，实际 %v", key, unreadTTL, renewed)
		}
	}
}

// TestSeqAllocateAfterExpiry 验证序号键过期后仍从库中最大值继续，不回到 1。
// 7 天 TTL 意味着冷会话一定会过期，这条路径是 TTL 的必然产物而非异常分支。
func TestSeqAllocateAfterExpiry(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	convID := "test:seq:expire:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, keySeq+convID) })

	const history = 42
	if _, err := allocate(ctx, c, convID, history); err != nil {
		t.Fatalf("初始化分配异常: %v", err)
	}

	// 模拟 TTL 到期：键被回收
	if err := c.rdb.Del(ctx, keySeq+convID).Err(); err != nil {
		t.Fatalf("模拟过期失败: %v", err)
	}
	got, err := allocate(ctx, c, convID, history)
	if err != nil {
		t.Fatalf("过期后分配异常: %v", err)
	}
	if got <= history {
		t.Fatalf("过期后序号必须越过历史最大值 %d，实际 %d", history, got)
	}
}

// TestSeqConcurrentInit 验证多个请求同时探测到空键时不会分配到重复序号。
// 这是「先探测再初始化」方案的核心保证：谁先跑脚本谁设 floor，其余只做 INCR。
func TestSeqConcurrentInit(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	convID := "test:seq:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, keySeq+convID) })

	const (
		workers = 50
		history = 1000
	)
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got = make([]int64, 0, workers)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := allocate(ctx, c, convID, history)
			if err != nil {
				t.Errorf("并发分配异常: %v", err)
				return
			}
			mu.Lock()
			got = append(got, seq)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(got) != workers {
		t.Fatalf("期望 %d 个序号，实际 %d", workers, len(got))
	}
	seen := make(map[int64]struct{}, workers)
	for _, seq := range got {
		if seq <= history {
			t.Fatalf("序号 %d 未越过历史最大值 %d", seq, history)
		}
		if _, ok := seen[seq]; ok {
			t.Fatalf("序号 %d 重复分配", seq)
		}
		seen[seq] = struct{}{}
	}
}
