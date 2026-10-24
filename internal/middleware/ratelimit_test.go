package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/cache"
)

// fakeStore 记录调用次数，用来断言「本地桶拒绝时不该打 Redis」。
type fakeStore struct {
	calls int
	state cache.RateState
	err   error
}

func (f *fakeStore) AllowSlide(context.Context, string, int64, time.Duration) (cache.RateState, error) {
	f.calls++
	return f.state, f.err
}

func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set(UIDKey, int64(1))
	return c, w
}

// TestLocalBucketRejectsBeforeRedis 验证两档的顺序：本地桶拒绝时不再打 Redis。
// 顺序反了会让每次超频请求都产生一次无效 Redis 往返。
func TestLocalBucketRejectsBeforeRedis(t *testing.T) {
	store := &fakeStore{state: cache.RateState{Allowed: true}}
	l := NewLimiter(true, store)
	rule := Rule{Name: "order", QPS: 1, Burst: 1, Global: 100, Window: time.Minute}

	c, _ := newTestContext()
	if !l.guard(c, rule, "u1") {
		t.Fatal("首次请求应放行")
	}
	if store.calls != 1 {
		t.Fatalf("首次应查询窗口后端 1 次，实际 %d", store.calls)
	}

	// 令牌桶已空，第二次必须被本地桶挡住，且不能再查 Redis
	c, _ = newTestContext()
	if l.guard(c, rule, "u1") {
		t.Fatal("令牌耗尽后应被拒绝")
	}
	if store.calls != 1 {
		t.Fatalf("本地桶拒绝后不该再查窗口后端，实际调用 %d 次", store.calls)
	}
}

// TestSlideWindowRejectsAfterLocalPass 验证总量闸生效，并带上完整的 429 头。
func TestSlideWindowRejectsAfterLocalPass(t *testing.T) {
	store := &fakeStore{state: cache.RateState{
		Allowed: false, Used: 120, RetryAfter: 3 * time.Second,
	}}
	l := NewLimiter(true, store)
	rule := Rule{Name: "write", QPS: 100, Burst: 100, Global: 120, Window: time.Minute}

	c, w := newTestContext()
	if l.guard(c, rule, "u1") {
		t.Fatal("窗口后端拒绝时整体应拒绝")
	}
	if got := w.Code; got != 429 {
		t.Fatalf("HTTP 状态应为 429，实际 %d", got)
	}
	want := map[string]string{
		"Retry-After":           "3",
		"X-RateLimit-Limit":     "120",
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Rule":      "write",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("响应头 %s 应为 %q，实际 %q", k, v, got)
		}
	}
}

// TestRedisFailureFallsBackToLocal 验证 Redis 报错时限流组件不阻断业务，
// 但仍受本地令牌桶约束。
func TestRedisFailureFallsBackToLocal(t *testing.T) {
	store := &fakeStore{err: errors.New("connection reset")}
	l := NewLimiter(true, store)
	rule := Rule{Name: "fallback", QPS: 1, Burst: 1, Global: 100, Window: time.Minute}

	c, _ := newTestContext()
	if !l.guard(c, rule, "u1") {
		t.Fatal("Redis 故障时不应阻断请求")
	}

	// 降级后令牌桶依然生效，避免「限流组件挂了 = 完全不设防」
	c, _ = newTestContext()
	if l.guard(c, rule, "u1") {
		t.Fatal("降级后仍应受本地令牌桶约束")
	}
}

// TestDisabledPassesThrough 验证开关关闭时全部放行且不碰后端。
// 走 ByUID 而不是直接调 guard：enabled 的短路在中间件入口，guard 只管判定。
func TestDisabledPassesThrough(t *testing.T) {
	store := &fakeStore{state: cache.RateState{Allowed: false}}
	l := NewLimiter(false, store)
	rule := Rule{Name: "off", QPS: 1, Burst: 1, Global: 1, Window: time.Minute}
	mw := l.ByUID(rule)

	for i := 0; i < 3; i++ {
		c, w := newTestContext()
		mw(c)
		if w.Code == 429 {
			t.Fatalf("限流关闭时第 %d 次不应被拒", i+1)
		}
	}
	if store.calls != 0 {
		t.Fatalf("限流关闭时不该查询后端，实际 %d 次", store.calls)
	}
}
