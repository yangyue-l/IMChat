package storage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// validConfig 是一份字段齐全的配置。
// 构造本身不发网络请求，所以这些用例不需要 OSS 凭据。
func validConfig() Config {
	return Config{
		Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "imchat-test",
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"缺 endpoint", func(c *Config) { c.Endpoint = "" }, "endpoint"},
		{"缺 bucket", func(c *Config) { c.Bucket = "" }, "bucket"},
		{"缺 AK", func(c *Config) { c.AccessKeyID = "" }, "凭据"},
		{"缺 SK", func(c *Config) { c.AccessKeySecret = "" }, "凭据"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mod(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatalf("%s 时应当报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案未点明原因: %v", err)
			}
		})
	}
}

// 请求已取消时不能再去打网络：这里只验证早退，不会真的发出请求。
func TestPutCanceledContext(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	url, err := st.Put(ctx, Object{Key: "u/1/2026/11/18/1.png", Body: strings.NewReader("x"), Size: 1})
	if err == nil {
		t.Fatal("已取消的上下文应当报错")
	}
	if url != "" {
		t.Fatalf("失败时不应返回 url，实际 %q", url)
	}
}

// 空列表不该发出请求，否则每次「没有旧图可删」的操作都会多一次 OSS 往返。
func TestDeleteWithoutKeys(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.Delete(context.Background()); err != nil {
		t.Fatalf("空列表应当直接返回 nil，实际 %v", err)
	}
}

// 删除同样要先看 context：请求已取消时不必再打一次网络。
func TestDeleteCanceledContext(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := st.Delete(ctx, "u/1/2026/11/18/1.png"); err == nil {
		t.Fatal("已取消的上下文应当报错")
	}
}

// newProbeStorage 构造一个只用于探活的实例：bucket 为 nil，
// probeFn 由用例替换，因此不会真的发出网络请求。
func newProbeStorage(t *testing.T, probeFn func() error) *ossStorage {
	t.Helper()
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := st.(*ossStorage)
	s.bucket = nil
	s.probeFn = probeFn
	return s
}

// ctx 先到就必须先返回：SDK 不接 context，若跟着它的 70s 超时一起等，
// /healthz 的 1s 探活上限形同虚设，负载均衡会看到「无响应」而不是「存储不可用」。
func TestPingReturnsOnContextDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	// 模拟一个卡住的 SDK 调用：只有测试结束时才放行
	s := newProbeStorage(t, func() error { <-release; return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.Ping(ctx)
	cost := time.Since(start)

	if err == nil {
		t.Fatal("探测卡住时应当按 ctx 截止时间返回错误")
	}
	if !strings.Contains(err.Error(), "探活超时") {
		t.Fatalf("错误文案应点明超时，实际 %v", err)
	}
	if cost > 2*time.Second {
		t.Fatalf("应当跟随 ctx 而非 SDK 超时，实际耗时 %s", cost)
	}
}

// 探活结果要缓存：负载均衡每隔几秒拉一次，实例一多就是成倍的 HEAD 请求。
func TestPingCachesResult(t *testing.T) {
	var calls atomic.Int64
	s := newProbeStorage(t, func() error {
		calls.Add(1)
		return nil
	})

	for i := 0; i < 5; i++ {
		if err := s.Ping(context.Background()); err != nil {
			t.Fatalf("第 %d 次探活不应报错: %v", i+1, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("缓存期内只应真打一次桶，实际 %d 次", got)
	}
}

// 失败结论同样要缓存：桶挂了的时候不该让每个 /healthz 都去撞一次。
func TestPingCachesError(t *testing.T) {
	boom := errors.New("boom")
	var calls atomic.Int64
	s := newProbeStorage(t, func() error {
		calls.Add(1)
		return boom
	})

	for i := 0; i < 3; i++ {
		err := s.Ping(context.Background())
		if !errors.Is(err, boom) {
			t.Fatalf("第 %d 次应返回原始错误，实际 %v", i+1, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("失败结论也应当缓存，实际打了 %d 次", got)
	}
}

// 并发探活只能打出一次 HEAD：同时来的 /healthz 共享同一个在飞的探测。
func TestPingSingleflight(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	s := newProbeStorage(t, func() error {
		calls.Add(1)
		<-release
		return nil
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Ping(context.Background())
		}(i)
	}

	// 等所有 goroutine 都进到「等结果」的状态再放行，否则它们会串成多轮探测
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个调用不应报错: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("并发探活只应真打一次桶，实际 %d 次", got)
	}
}

// ctxReader 是上传能被中断的前提：SDK 不接 context，
// 只有让下一次 Read 直接报错，取消才能真正停住一个正在传的 10MB 文件。
func TestCtxReaderStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ctxReader{ctx: ctx, r: strings.NewReader(strings.Repeat("x", 1024))}

	buf := make([]byte, 16)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("未取消时应正常读取: %v", err)
	}

	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后 Read 应返回 context.Canceled，实际 %v", err)
	}
}
