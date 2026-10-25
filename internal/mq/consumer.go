package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"imchat/internal/config"
	"imchat/internal/service"
)

// maxRetry 是单条消息的最大投递次数（含首次），超过后进入死信队列人工排查。
const maxRetry = 3

// Consumer 消费事件队列，手动 ACK 保证消息不丢，异常时按次数重试或转死信。
//
// 并发模型：每个 worker 独占一条 channel（各自 Qos + Consume），互不共享。
// amqp091 的 channel 不是并发安全的，共享一条 channel 就必须串行化，
// 那正好退化成单线程消费——所以并发度是靠多 channel 换来的，不是多 goroutine 抢一条 channel。
type Consumer struct {
	cfg      config.RabbitMQConfig
	registry *Registry
	log      *slog.Logger
	wg       sync.WaitGroup
	cancel   context.CancelFunc
}

// NewConsumer 创建消费者。
func NewConsumer(cfg config.RabbitMQConfig, registry *Registry, log *slog.Logger) *Consumer {
	return &Consumer{cfg: cfg, registry: registry, log: log}
}

// Start 在后台启动消费循环，断线自动重连。
func (c *Consumer) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		backoff := initialBackoff
		for {
			if err := c.run(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("RabbitMQ 消费中断，稍后重连", "err", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < maxBackoff {
					backoff *= 2
				}
				continue
			}
			return
		}
	}()
}

// workers 返回生效的消费并发度。
func (c *Consumer) workers() int {
	if c.cfg.ConsumerWorkers > 0 {
		return c.cfg.ConsumerWorkers
	}
	return 1
}

// run 建立一次消费会话，直到连接断开、任一 worker 掉线或上下文取消。
func (c *Consumer) run(ctx context.Context) error {
	conn, err := dialAMQP(c.cfg.URL)
	if err != nil {
		return fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	defer func() { _ = conn.Close() }()

	types := c.registry.Types()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 拓扑与路由绑定只做一次：交换机、队列、绑定都是 broker 侧全局状态，
	// 每条 worker channel 重复声明纯属浪费 RPC（workers 配到 32 时就是 64 次）。
	setup, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("创建拓扑 channel 失败: %w", err)
	}
	if err := declareTopology(setup); err != nil {
		_ = setup.Close()
		return err
	}
	if err := BindEvents(setup, types); err != nil {
		_ = setup.Close()
		return err
	}
	_ = setup.Close()

	workers := c.workers()
	// 缓冲到 worker 数，保证任何一个 worker 报错都不会被自己阻塞住
	errCh := make(chan error, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		ch, err := conn.Channel()
		if err != nil {
			cancel()
			wg.Wait()
			return fmt.Errorf("创建消费 channel 失败: %w", err)
		}
		// Qos 是 channel 级的：未确认消息总量 = Prefetch × workers
		if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
			_ = ch.Close()
			cancel()
			wg.Wait()
			return fmt.Errorf("设置 prefetch 失败: %w", err)
		}
		deliveries, err := ch.Consume(QueueName, "", false, false, false, false, nil)
		if err != nil {
			_ = ch.Close()
			cancel()
			wg.Wait()
			return fmt.Errorf("订阅队列失败: %w", err)
		}

		wg.Add(1)
		go func(idx int, ch *amqp.Channel, d <-chan amqp.Delivery) {
			defer wg.Done()
			defer func() { _ = ch.Close() }()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-d:
					if !ok {
						errCh <- fmt.Errorf("消费通道 %d 已关闭", idx)
						return
					}
					c.handle(ctx, ch, msg)
				}
			}
		}(i, ch, deliveries)
	}
	c.log.Info("RabbitMQ 消费者已启动", "queue", QueueName, "events", types,
		"workers", workers, "prefetch", c.cfg.Prefetch)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		// 一个 worker 掉线就整轮重来，避免长期只有部分并发在跑
		cancel()
	}
	wg.Wait()
	return runErr
}

// handle 处理单条消息：成功 ACK；失败重投并累加 x-retry；超限转死信。
// 注意：requeue=true 不会修改 header，因此重试必须重新发布消息，否则计数永远为 0 会无限重试。
func (c *Consumer) handle(ctx context.Context, ch *amqp.Channel, d amqp.Delivery) {
	var evt service.Event
	if err := json.Unmarshal(d.Body, &evt); err != nil {
		c.log.Warn("事件反序列化失败，直接丢弃", "err", err)
		_ = d.Reject(false)
		return
	}

	retry := retryOf(d.Headers)

	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := c.registry.Dispatch(hctx, evt)
	cancel()

	if err == nil {
		_ = d.Ack(false)
		return
	}

	if retry+1 >= maxRetry {
		c.log.Error("事件处理连续失败，转入死信队列", "type", evt.Type, "retry", retry, "err", err)
		_ = d.Reject(false) // 不重新入队，由 DLX 接管
		return
	}

	c.log.Warn("事件处理失败，准备重试", "type", evt.Type, "retry", retry+1, "err", err)
	if err := c.republish(ctx, ch, d, retry+1); err != nil {
		c.log.Error("重投失败，交回 broker", "err", err)
		_ = d.Nack(false, true)
		return
	}
	_ = d.Ack(false)
}

// retryOf 读出已尝试次数。
//
// 自己生产的是 int32，但 amqp 表里的整数类型由**发送方**决定：别的语言客户端
// 常发 int64，JSON 中转的甚至会是 float64。只断言一种类型的话，取不到就退化成 0，
// 重试计数永远归零 —— 等于无限重试。
func retryOf(headers amqp.Table) int {
	var n int
	switch v := headers["x-retry"].(type) {
	case int:
		n = v
	case int8:
		n = int(v)
	case int16:
		n = int(v)
	case int32:
		n = int(v)
	case int64:
		n = int(v)
	case uint8:
		n = int(v)
	case uint16:
		n = int(v)
	case uint32:
		n = int(v)
	case uint64:
		n = int(v)
	case float32:
		n = int(v)
	case float64:
		n = int(v)
	}
	if n < 0 {
		return 0
	}
	return n
}

// republish 带累加的重试计数重新投递。
func (c *Consumer) republish(ctx context.Context, ch *amqp.Channel, d amqp.Delivery, retry int) error {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers["x-retry"] = int32(retry)

	return ch.PublishWithContext(ctx, ExchangeName, d.RoutingKey, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Type:         d.Type,
		Headers:      headers,
		Body:         d.Body,
	})
}

// Close 停止消费并等待退出。
func (c *Consumer) Close() {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
}
