// Package mq 封装 RabbitMQ 的生产与消费，并提供本地内存总线兜底。
// 同一套 Handler 注册表既可供 RabbitMQ 消费，也可在 MQ 未启用时本地异步执行。
package mq

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"imchat/internal/service"
)

// Handler 处理一类事件。
type Handler func(ctx context.Context, data json.RawMessage) error

// errBusClosed 表示总线已停止消费。调用方据此判断事件是否真的投出去了。
var errBusClosed = errors.New("本地总线已关闭")

// Registry 维护事件类型到处理函数的映射，并发安全。
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register 注册处理器，同一类型重复注册会覆盖。
func (r *Registry) Register(eventType string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[eventType] = h
}

// Dispatch 分发事件；未注册的类型直接忽略。
func (r *Registry) Dispatch(ctx context.Context, evt service.Event) error {
	r.mu.RLock()
	h, ok := r.handlers[evt.Type]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return h(ctx, evt.Data)
}

// Types 返回已注册的事件类型，用于绑定路由。
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		out = append(out, t)
	}
	return out
}

// LocalBus 是 MQ 未启用时的兜底实现：单 worker 串行消费，缓冲满则丢弃并告警。
// 它保证业务代码不感知 MQ 是否可用，也避免了 goroutine 无限膨胀。
type LocalBus struct {
	registry *Registry
	ch       chan service.Event
	log      *slog.Logger
	dropped  int64
	mu       sync.Mutex
	wg       sync.WaitGroup
	closed   chan struct{}
	once     sync.Once
}

// NewLocalBus 创建本地总线，size 为缓冲容量。
func NewLocalBus(registry *Registry, size int, log *slog.Logger) *LocalBus {
	if size <= 0 {
		size = 1024
	}
	return &LocalBus{
		registry: registry,
		ch:       make(chan service.Event, size),
		log:      log,
		closed:   make(chan struct{}),
	}
}

// Publish 非阻塞投递，队列满时丢弃并计数，避免拖慢业务主流程。
//
// 关闭判定与入队必须在同一把锁内完成。只查一次然后投递会留下窗口：判定时还没关，
// 投递时消费协程已经退出，事件躺在缓冲区里没人处理，调用方却收到 nil。
// 同理，单个 select 里同时摆 `b.ch <- evt` 与 `<-b.closed` 也不够——两者都就绪时
// Go 随机挑一个，关闭后仍有一半概率"投递成功"，得靠容量填满才稳定报错。
func (b *LocalBus) Publish(_ context.Context, evt service.Event) error {
	b.mu.Lock()
	select {
	case <-b.closed:
		b.mu.Unlock()
		return errBusClosed
	default:
	}
	// 发送本身带 default，持锁不会等待消费者；锁只在投递与关闭之间竞争。
	select {
	case b.ch <- evt:
		b.mu.Unlock()
		return nil
	default:
	}
	b.dropped++
	dropped := b.dropped
	b.mu.Unlock()

	// 日志放锁外：slog 要落盘，放在锁内会把所有投递路径串成一条。
	if dropped%100 == 1 {
		b.log.Warn("本地事件队列已满，事件被丢弃", "type", evt.Type, "dropped_total", dropped)
	}
	return nil
}

// Start 启动消费循环。并发度由 worker 数决定，本地总线默认 1 个，保证顺序。
func (b *LocalBus) Start(workers int) {
	if workers <= 0 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for {
				select {
				case <-b.closed:
					return
				case evt := <-b.ch:
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := b.registry.Dispatch(ctx, evt); err != nil {
						b.log.Warn("本地事件处理失败", "type", evt.Type, "err", err)
					}
					cancel()
				}
			}
		}()
	}
}

// Close 停止消费并等待退出。
func (b *LocalBus) Close() {
	// 关 channel 也在同一把锁内：Publish 侧靠它保证「判完到投递」之间不会被关。
	b.once.Do(func() {
		b.mu.Lock()
		close(b.closed)
		b.mu.Unlock()
	})
	b.wg.Wait()
}
