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

// 交换机与队列命名。
const (
	ExchangeName = "im.events"
	DLXName      = "im.dlx"
	QueueName    = "im.events.q"
	DLQName      = "im.events.dlq"
)

// Producer 是 RabbitMQ 生产者。channel 非并发安全，因此维护一个 channel 池，
// 每个 Publish 独占一个 channel，避免加锁串行化。
type Producer struct {
	cfg config.RabbitMQConfig
	log *slog.Logger

	mu   sync.RWMutex
	conn *amqp.Connection
	pool chan *amqp.Channel
}

// NewProducer 建立连接、声明拓扑并预热 channel 池。
func NewProducer(cfg config.RabbitMQConfig, log *slog.Logger) (*Producer, error) {
	p := &Producer{
		cfg:  cfg,
		log:  log,
		pool: make(chan *amqp.Channel, cfg.ChannelPool),
	}
	if err := p.connect(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Producer) connect() error {
	conn, err := amqp.Dial(p.cfg.URL)
	if err != nil {
		return fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("创建 RabbitMQ channel 失败: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		_ = conn.Close()
		return err
	}
	if p.cfg.PublisherConfirm {
		if err := ch.Confirm(false); err != nil {
			_ = conn.Close()
			return fmt.Errorf("开启 publisher confirm 失败: %w", err)
		}
	}
	_ = ch.Close()

	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	// 预热
	for i := 0; i < p.cfg.ChannelPool; i++ {
		c, err := conn.Channel()
		if err != nil {
			break
		}
		if p.cfg.PublisherConfirm {
			if err := c.Confirm(false); err != nil {
				_ = c.Close()
				break
			}
		}
		select {
		case p.pool <- c:
		default:
			_ = c.Close()
		}
	}
	p.log.Info("RabbitMQ 生产者就绪", "exchange", ExchangeName, "channels", len(p.pool))
	return nil
}

// Publish 投递事件到交换机，routing key 使用事件类型。
func (p *Producer) Publish(ctx context.Context, evt service.Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("序列化事件失败: %w", err)
	}

	ch, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	ok := false
	defer func() { p.release(ch, ok) }()

	msg := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent, // 持久化，broker 重启不丢事件
		Timestamp:    time.Now(),
		Type:         evt.Type,
		Body:         body,
	}

	if p.cfg.PublisherConfirm {
		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, ExchangeName, evt.Type, false, false, msg)
		if err != nil {
			return fmt.Errorf("投递事件失败: %w", err)
		}
		acked, err := conf.WaitContext(ctx)
		if err != nil {
			return fmt.Errorf("等待 broker 确认失败: %w", err)
		}
		if !acked {
			return fmt.Errorf("broker 未确认事件 %s", evt.Type)
		}
		ok = true
		return nil
	}

	if err := ch.PublishWithContext(ctx, ExchangeName, evt.Type, false, false, msg); err != nil {
		return fmt.Errorf("投递事件失败: %w", err)
	}
	ok = true
	return nil
}

// acquire 取一个可用 channel；池空时临时新建，保证高并发下不被池大小卡死。
func (p *Producer) acquire(ctx context.Context) (*amqp.Channel, error) {
	select {
	case ch := <-p.pool:
		if ch != nil && !ch.IsClosed() {
			return ch, nil
		}
		return p.newChannel()
	default:
		return p.newChannel()
	}
}

func (p *Producer) newChannel() (*amqp.Channel, error) {
	p.mu.RLock()
	conn := p.conn
	p.mu.RUnlock()
	if conn == nil || conn.IsClosed() {
		return nil, fmt.Errorf("RabbitMQ 连接不可用")
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("创建 channel 失败: %w", err)
	}
	if p.cfg.PublisherConfirm {
		if err := ch.Confirm(false); err != nil {
			_ = ch.Close()
			return nil, fmt.Errorf("开启 confirm 失败: %w", err)
		}
	}
	return ch, nil
}

// release 归还 channel；失败时直接关闭，避免把坏 channel 放回池里。
func (p *Producer) release(ch *amqp.Channel, healthy bool) {
	if !healthy || ch.IsClosed() {
		_ = ch.Close()
		return
	}
	select {
	case p.pool <- ch:
	default:
		_ = ch.Close()
	}
}

// Close 关闭连接与全部 channel。
func (p *Producer) Close() {
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	p.mu.Unlock()

	for {
		select {
		case ch := <-p.pool:
			if ch != nil {
				_ = ch.Close()
			}
		default:
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
	}
}

// declareTopology 声明交换机、队列、死信与绑定关系。幂等，可重复调用。
func declareTopology(ch *amqp.Channel) error {
	// 死信交换与队列，用于承接多次重试仍失败的事件
	if err := ch.ExchangeDeclare(DLXName, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("声明死信交换机失败: %w", err)
	}
	if _, err := ch.QueueDeclare(DLQName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("声明死信队列失败: %w", err)
	}
	if err := ch.QueueBind(DLQName, "dead", DLXName, false, nil); err != nil {
		return fmt.Errorf("绑定死信队列失败: %w", err)
	}

	if err := ch.ExchangeDeclare(ExchangeName, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("声明事件交换机失败: %w", err)
	}
	if _, err := ch.QueueDeclare(QueueName, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    DLXName,
		"x-dead-letter-routing-key": "dead",
	}); err != nil {
		return fmt.Errorf("声明事件队列失败: %w", err)
	}
	return nil
}

// BindEvents 把事件类型绑定到队列，消费者启动时调用。
func BindEvents(ch *amqp.Channel, types []string) error {
	for _, t := range types {
		if err := ch.QueueBind(QueueName, t, ExchangeName, false, nil); err != nil {
			return fmt.Errorf("绑定事件 %s 失败: %w", t, err)
		}
	}
	return nil
}
