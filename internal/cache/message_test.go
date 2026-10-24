package cache

import (
	"context"
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
