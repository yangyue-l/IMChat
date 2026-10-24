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
func (b *LocalBus) Publish(_ context.Context, evt service.Event) error {
	select {
	case b.ch <- evt:
		return nil
	case <-b.closed:
		return errors.New("本地总线已关闭")
	default:
		b.mu.Lock()
		b.dropped++
		dropped := b.dropped
		b.mu.Unlock()
		if dropped%100 == 1 {
			b.log.Warn("本地事件队列已满，事件被丢弃", "type", evt.Type, "dropped_total", dropped)
		}
		return nil
	}
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
	b.once.Do(func() { close(b.closed) })
	b.wg.Wait()
}
