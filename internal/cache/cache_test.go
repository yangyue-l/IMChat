package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"imchat/internal/config"
)

// rememberKey 为每条用例分配独立 Key，避免用例之间互相污染。
func rememberKey(t *testing.T) string {
	t.Helper()
	return "test:remember:" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// TestRememberMergesConcurrentMisses 验证同一 Key 的并发未命中只回源一次。
//
// 钉住的是「回源风暴」这个失败模式：热点 Key 刚失效时 N 个请求会同时穿透到 DB，
// 一次热点读就打出 N 条慢查询。singleflight 合并的正是这段窗口，但它是纯并发的 ——
// 把 sf.Do 换回直接 load，功能上完全看不出问题，只有并发下的回源计数能发现。
func TestRememberMergesConcurrentMisses(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := rememberKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		// 故意拖住第一个回源：其余协程必须在这段窗口内进入 singleflight 等待，
		// 否则它们会在回源完成后直接命中缓存，用例退化成 10 次顺序读、测不出合并。
		// 这里等的是「并发窗口」，不是断言耗时。
		time.Sleep(100 * time.Millisecond)
		return []byte(`{"id":1}`), nil
	}

	const workers = 10
	const want = `{"id":1}`

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			raw, err := c.Remember(ctx, key, time.Minute, load)
			if err != nil {
				errs <- fmt.Errorf("并发读取异常: %w", err)
				return
			}
			if string(raw) != want {
				errs <- fmt.Errorf("并发读取内容期望 %s，实际 %q", want, raw)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if got := loads.Load(); got != 1 {
		t.Fatalf("%d 个并发未命中应只回源 1 次，实际 %d 次", workers, got)
	}

	// 合并的是回源，不是写缓存：值必须真的落进 Redis，否则下一次读仍会回源，
	// 「只回源一次」就只是这一波的巧合。
	cached, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("回源成功后应写入缓存，读取失败: %v", err)
	}
	if cached != want {
		t.Fatalf("缓存内容期望 %s，实际 %q", want, cached)
	}

	// 命中路径不该再触发回源
	if _, err := c.Remember(ctx, key, time.Minute, load); err != nil {
		t.Fatalf("二次读取异常: %v", err)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("缓存命中后不应回源，实际累计回源 %d 次", got)
	}
}

// TestRememberNullMarkerStopsPenetration 验证回源返回 ErrNotFound 时写入空值占位，
// 后续读取直接由缓存挡住，不再回源。
func TestRememberNullMarkerStopsPenetration(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := rememberKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(ctx, key) })

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return nil, ErrNotFound
	}

	if _, err := c.Remember(ctx, key, time.Minute, load); !errors.Is(err, ErrNotFound) {
		t.Fatalf("首次回源应透传 ErrNotFound，实际 %v", err)
	}

	// 占位值必须带 TTL：永不过期的话，数据后来真的写进库了也永远读不到，
	// 比穿透更糟 —— 穿透只是压 DB，占位不失效是「假空」长期存在。
	// -1 表示永不过期，-2 表示根本没有这个 Key。
	ttl, err := c.rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取占位 Key 的 TTL 异常: %v", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("空值占位的 TTL 应为 (0, 1m]，实际 %v（-1 表示永不过期，-2 表示占位 Key 不存在）", ttl)
	}

	if _, err := c.Remember(ctx, key, time.Minute, load); !errors.Is(err, ErrNotFound) {
		t.Fatalf("命中占位后应返回 ErrNotFound，实际 %v", err)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("占位命中后不应回源，实际累计回源 %d 次", got)
	}
}

// TestRememberFallsBackWhenRedisDown 验证 Redis 不可用时直接回源，不阻断主流程。
//
// 这条用例要的恰恰是「连不上」，所以不走 newTestCache（那会在 Redis 离线时整体 skip），
// 而是拿一个确定无人监听的地址去连 —— 没有 Redis 的环境也必须能真实跑起来，
// 否则「降级」这条最关键的分支永远得不到验证。
func TestRememberFallsBackWhenRedisDown(t *testing.T) {
	// 先占一个端口再立刻释放，保证地址上没有任何监听者；比写死 127.0.0.1:1
	// 更稳（低端口在某些环境会被特权服务占用，或者被防火墙长时间丢弃）。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("释放端口失败: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1, // 禁用重试：重试只会把「不可用」放大成秒级延迟
	})
	t.Cleanup(func() { _ = rdb.Close() })
	c := New(rdb, config.CacheConfig{})

	var loads atomic.Int32
	load := func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("db"), nil
	}

	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		raw, err := c.Remember(ctx, "test:remember:down", time.Minute, load)
		if err != nil {
			t.Fatalf("第 %d 次降级回源异常: %v", i, err)
		}
		if string(raw) != "db" {
			t.Fatalf("第 %d 次降级回源内容期望 db，实际 %q", i, raw)
		}
	}

	// 缓存不可用时没有共享介质，每次调用都回源 —— 这是有意的取舍：
	// 宁可放大 DB 压力，也不能让健康检查通过、业务全 500。
	if got := loads.Load(); got != 2 {
		t.Fatalf("Redis 不可用时每次调用都应回源，期望 2 次，实际 %d 次", got)
	}
}
