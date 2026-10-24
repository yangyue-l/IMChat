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

// maxRetry 是单条消息的最大本地重试次数，超过后进入死信队列人工排查。
const maxRetry = 3

// Consumer 消费事件队列，手动 ACK 保证消息不丢，异常时按次数重试或转死信。
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
		backoff := time.Second
		for {
			if err := c.run(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("RabbitMQ 消费中断，稍后重连", "err", err, "backoff", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			return
		}
	}()
}

// run 建立一次消费会话，直到连接断开或上下文取消。
func (c *Consumer) run(ctx context.Context) error {
	conn, err := amqp.Dial(c.cfg.URL)
	if err != nil {
		return fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("创建消费 channel 失败: %w", err)
	}
	defer func() { _ = ch.Close() }()

	if err := declareTopology(ch); err != nil {
		return err
	}
	types := c.registry.Types()
	if err := BindEvents(ch, types); err != nil {
		return err
	}
	// 按处理能力预取，避免一次性推送过多消息压垮单实例
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("设置 prefetch 失败: %w", err)
	}

	deliveries, err := ch.Consume(QueueName, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("订阅队列失败: %w", err)
	}
	c.log.Info("RabbitMQ 消费者已启动", "queue", QueueName, "events", types, "prefetch", c.cfg.Prefetch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("消费通道已关闭")
			}
			c.handle(ctx, ch, d)
		}
	}
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

	retry := 0
	if v, ok := d.Headers["x-retry"].(int32); ok {
		retry = int(v)
	}

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
