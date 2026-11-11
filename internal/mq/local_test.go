package mq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"imchat/internal/service"
)

// testLog 丢掉日志：丢弃分支会打 Warn，写进 stderr 会淹没失败输出。
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noopHandler(context.Context, json.RawMessage) error { return nil }

// TestRegistryRegisterOverwrites 锁住「同类型重复注册覆盖」的语义。
//
// 事件类型是字符串字面量，两个包各自注册同一个类型不会报错也不会 panic，
// 只会静默地少一半处理器被调用 —— 覆盖是既定行为，得有用例钉住它。
func TestRegistryRegisterOverwrites(t *testing.T) {
	r := NewRegistry()
	var first, second atomic.Int64
	r.Register("e", func(context.Context, json.RawMessage) error {
		first.Add(1)
		return nil
	})
	r.Register("e", func(context.Context, json.RawMessage) error {
		second.Add(1)
		return nil
	})

	if err := r.Dispatch(context.Background(), service.Event{Type: "e"}); err != nil {
		t.Fatalf("Dispatch 返回错误: %v", err)
	}
	if got := first.Load(); got != 0 {
		t.Fatalf("旧处理器被调用 %d 次，重复注册应当覆盖而不是叠加", got)
	}
	if got := second.Load(); got != 1 {
		t.Fatalf("新处理器调用 %d 次，期望 1", got)
	}
	if got := len(r.Types()); got != 1 {
		t.Fatalf("Types 长度 = %d，同一类型只应登记一次", got)
	}
}

// TestRegistryTypesSnapshot 锁住 Types 的快照语义。
//
// 消费者拿 Types 的返回值去绑定路由，之后业务可能继续 Register；若返回的是
// 内部 map 的视图或共享切片，绑定中途被改会得到一份不完整的订阅关系。
func TestRegistryTypesSnapshot(t *testing.T) {
	r := NewRegistry()
	r.Register("a", noopHandler)

	snapshot := r.Types()
	r.Register("b", noopHandler)

	if len(snapshot) != 1 {
		t.Fatalf("已返回的快照长度变成 %d，后续注册不应影响它", len(snapshot))
	}

	// 改返回值不能反过来污染注册表
	snapshot[0] = "mutated"
	types := r.Types()
	if len(types) != 2 {
		t.Fatalf("Types 长度 = %d，期望 2", len(types))
	}
	for _, name := range types {
		if name == "mutated" {
			t.Fatal("修改 Types 的返回值污染了注册表")
		}
	}
}

// TestRegistryDispatch 覆盖分发出口的两种结果。
//
// 未注册类型返回 nil 而不是错误：生产端与服务端可能滚动升级，新事件类型
// 落到老节点时应当安静丢弃，否则每次发布都会触发重试直到进死信。
func TestRegistryDispatch(t *testing.T) {
	r := NewRegistry()

	t.Run("未注册类型直接忽略", func(t *testing.T) {
		if err := r.Dispatch(context.Background(), service.Event{Type: "absent"}); err != nil {
			t.Fatalf("未注册类型应返回 nil，实际 %v", err)
		}
	})

	t.Run("处理器错误原样上抛", func(t *testing.T) {
		wantErr := errors.New("处理器失败")
		r.Register("bad", func(context.Context, json.RawMessage) error { return wantErr })
		if err := r.Dispatch(context.Background(), service.Event{Type: "bad"}); !errors.Is(err, wantErr) {
			t.Fatalf("错误未原样上抛，实际 %v", err)
		}
	})

	t.Run("载荷原样传给处理器", func(t *testing.T) {
		r.Register("payload", func(_ context.Context, data json.RawMessage) error {
			if string(data) != `{"v":1}` {
				return errors.New("载荷不一致: " + string(data))
			}
			return nil
		})
		if err := r.Dispatch(context.Background(), service.Event{
			Type: "payload", Data: json.RawMessage(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Dispatch 返回错误: %v", err)
		}
	})
}

// TestLocalBusDropsInsteadOfBlocking 锁住队列满时的「丢弃而不阻塞」。
//
// 这是本地总线存在的唯一理由：MQ 不可用时业务主流程不能被事件队列拖住。
// 用例不启动消费协程，队列填满后所有投递只能走丢弃分支；用 watchdog 而不是
// 比较耗时，阻塞与否是二值事实，不需要靠时间余量判定。
func TestLocalBusDropsInsteadOfBlocking(t *testing.T) {
	const size = 2
	bus := NewLocalBus(NewRegistry(), size, testLog())
	defer bus.Close()

	for i := 0; i < size; i++ {
		if err := bus.Publish(context.Background(), service.Event{Type: "e"}); err != nil {
			t.Fatalf("第 %d 条投递失败: %v", i, err)
		}
	}

	const extra = 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < extra; i++ {
			_ = bus.Publish(context.Background(), service.Event{Type: "e"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("队列满时 Publish 阻塞了，业务主流程会被事件总线拖住")
	}

	bus.mu.Lock()
	dropped := bus.dropped
	bus.mu.Unlock()
	if dropped != extra {
		t.Fatalf("丢弃计数 = %d，期望 %d", dropped, extra)
	}
}

// TestLocalBusPublishAfterCloseFails 锁住关闭后的投递必须报错。
//
// 关闭后缓冲区往往是空的，此时「投递成功」是谎报：消费协程已退出，
// 事件躺在 channel 里不会被任何人处理，而调用方以为送达了。
func TestLocalBusPublishAfterCloseFails(t *testing.T) {
	bus := NewLocalBus(NewRegistry(), 64, testLog())
	bus.Start(1)
	bus.Close()

	const n = 100
	accepted := 0
	for i := 0; i < n; i++ {
		err := bus.Publish(context.Background(), service.Event{Type: "e"})
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, errBusClosed) {
			t.Fatalf("关闭后的错误应当是 errBusClosed，实际 %v", err)
		}
	}
	if accepted != 0 {
		t.Fatalf("关闭后仍有 %d/%d 次投递被接受", accepted, n)
	}
}

// TestLocalBusStartZeroRunsOneWorker 锁住 Start(0) 退化为 1 个 worker。
//
// 两个方向都要钉住：worker 数为 0 会让事件永不处理（队列静默积满后全丢弃），
// 大于 1 则丢掉本地总线「串行消费保证顺序」的前提。用「最大并发」判定后者，
// 处理器故意睡一会儿，两个 worker 必然重叠。
func TestLocalBusStartZeroRunsOneWorker(t *testing.T) {
	registry := NewRegistry()
	var mu sync.Mutex
	var inflight, peak, handled int
	registry.Register("e", func(context.Context, json.RawMessage) error {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		inflight--
		handled++
		mu.Unlock()
		return nil
	})

	bus := NewLocalBus(registry, 64, testLog())
	bus.Start(0)

	const total = 8
	for i := 0; i < total; i++ {
		if err := bus.Publish(context.Background(), service.Event{Type: "e"}); err != nil {
			t.Fatalf("第 %d 条投递失败: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := handled
		mu.Unlock()
		if done == total {
			break
		}
		if time.Now().After(deadline) {
			bus.Close()
			t.Fatalf("5s 内只处理了 %d/%d 条，Start(0) 必须启动 1 个 worker", done, total)
		}
		time.Sleep(5 * time.Millisecond)
	}

	bus.Close()
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak != 1 {
		t.Fatalf("最大并发 = %d，Start(0) 只应启动 1 个 worker", gotPeak)
	}
}

// TestLocalBusCloseIsIdempotent 锁住重复 Close 不 panic。
//
// Close 会走两条路径：正常停机与启动失败回滚，两条都执行到就是两次 Close；
// 直接 close channel 第二次会 panic 在 defer 里，把真正的启动错误盖掉。
func TestLocalBusCloseIsIdempotent(t *testing.T) {
	bus := NewLocalBus(NewRegistry(), 4, testLog())
	bus.Start(1)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("重复 Close 触发 panic: %v", r)
		}
	}()
	bus.Close()
	bus.Close()
}
