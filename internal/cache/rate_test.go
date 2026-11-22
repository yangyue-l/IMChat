package cache

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"imchat/internal/config"
)

// newTestCache 连接本机 Redis，连不上就跳过，避免无依赖环境下误报失败。
func newTestCache(t *testing.T) *Cache {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("本机 Redis 不可用，跳过: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, config.CacheConfig{})
}

func testKey(t *testing.T) string {
	t.Helper()
	return "test:slide:" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// TestAllowSlideQuota 验证窗口内的放行次数严格等于 limit。
func TestAllowSlideQuota(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := testKey(t)

	const (
		limit  = 5
		window = time.Second
	)
	for i := 1; i <= limit; i++ {
		st, err := c.AllowSlide(ctx, key, limit, window)
		if err != nil {
			t.Fatalf("第 %d 次调用异常: %v", i, err)
		}
		if !st.Allowed {
			t.Fatalf("第 %d 次应放行，实际被拒", i)
		}
		if st.Used != int64(i) {
			t.Fatalf("第 %d 次计数应为 %d，实际 %d", i, i, st.Used)
		}
	}

	st, err := c.AllowSlide(ctx, key, limit, window)
	if err != nil {
		t.Fatalf("超限调用异常: %v", err)
	}
	if st.Allowed {
		t.Fatal("超过 limit 后应被拒绝")
	}
	if st.RetryAfter <= 0 || st.RetryAfter > window {
		t.Fatalf("RetryAfter 应落在 (0, %v]，实际 %v", window, st.RetryAfter)
	}

	// 同一窗口内重复请求必须持续被拒，不能因为拒绝就放出额度
	if st2, _ := c.AllowSlide(ctx, key, limit, window); st2.Allowed {
		t.Fatal("窗口内重复请求不应放行")
	} else if st2.Used > limit {
		t.Fatalf("被拒请求不该写入窗口，Used=%d 超过 limit=%d", st2.Used, limit)
	}
}

// TestAllowSlideRecovers 验证记录滑出窗口后额度恢复。
func TestAllowSlideRecovers(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := testKey(t)

	const (
		limit  = 2
		window = 300 * time.Millisecond
	)
	for i := 0; i < limit; i++ {
		if st, _ := c.AllowSlide(ctx, key, limit, window); !st.Allowed {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if st, _ := c.AllowSlide(ctx, key, limit, window); st.Allowed {
		t.Fatal("额度用完应被拒绝")
	}

	time.Sleep(window + 150*time.Millisecond)
	if st, _ := c.AllowSlide(ctx, key, limit, window); !st.Allowed {
		t.Fatal("滑出窗口后应恢复放行")
	}
}

// TestAllowSlideNoBurstAtBoundary 是滑动窗口相对固定窗口的核心差异。
//
// 固定窗口在 key 过期重建的瞬间会重置计数：在窗口尾部和下一个窗口头部
// 各打满一次，短时间内的实际通过量可达 2×limit。滑动窗口按「最近一个窗口」
// 统计，同样的时序下第二次必须被拒。
func TestAllowSlideNoBurstAtBoundary(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := testKey(t)

	const (
		limit  = 3
		window = 2 * time.Second
	)

	// ① 窗口起点先消费 1 次，固定窗口的 TTL 从这里开始计时
	if st, _ := c.AllowSlide(ctx, key, limit, window); !st.Allowed {
		t.Fatal("首次请求应放行")
	}

	// ② 临近窗口尾部再消费 2 次，此时恰好用满 limit
	time.Sleep(window - 200*time.Millisecond)
	for i := 0; i < 2; i++ {
		if st, _ := c.AllowSlide(ctx, key, limit, window); !st.Allowed {
			t.Fatalf("窗口尾部第 %d 次应放行", i+1)
		}
	}

	// ③ 跨过固定窗口的重置点，此时滑动窗口里仍有 ② 的两条记录
	time.Sleep(300 * time.Millisecond)

	if st, _ := c.AllowSlide(ctx, key, limit, window); !st.Allowed {
		t.Fatal("跨边界后的第一次应放行（窗口内尚余 1 个额度）")
	}
	st, _ := c.AllowSlide(ctx, key, limit, window)
	if st.Allowed {
		t.Fatal("跨边界后第二次必须被拒：最近一个窗口内已达 limit，放行即为固定窗口的双倍额度突刺")
	}
}

// TestAllowSlideRecoverWrongType 覆盖从固定窗口升级过来的场景。
// 旧版本把限流键写成 string，同名键的 zset 命令会直接 WRONGTYPE；
// 这里要求自动清掉旧键重建，而不是让接口整体 500。
func TestAllowSlideRecoverWrongType(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()
	key := testKey(t)

	if err := c.rdb.Set(ctx, keyRate+key, "legacy-counter", time.Minute).Err(); err != nil {
		t.Fatalf("预置旧键失败: %v", err)
	}

	st, err := c.AllowSlide(ctx, key, 3, time.Second)
	if err != nil {
		t.Fatalf("遇到旧类型键应自动恢复，实际报错: %v", err)
	}
	if !st.Allowed {
		t.Fatal("重建后首次请求应放行")
	}
	if typ := c.rdb.Type(ctx, keyRate+key).Val(); typ != "zset" {
		t.Fatalf("键应被重建为 zset，实际 %s", typ)
	}
}
