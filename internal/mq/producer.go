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

// 建连与重连参数，生产者与消费者共用。
const (
	// dialTimeout 限制建连耗时。amqp.Dial 本身没有超时，broker 半死不活时
	// 一次 Dial 能挂几十秒，既拖住启动也会拖住优雅停机。
	dialTimeout = 5 * time.Second
	// initialBackoff / maxBackoff 是重连退避区间。
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// dialAMQP 带超时地建立 AMQP 连接。
func dialAMQP(url string) (*amqp.Connection, error) {
	return amqp.DialConfig(url, amqp.Config{Dial: amqp.DefaultDial(dialTimeout)})
}

// Producer 是 RabbitMQ 生产者。channel 非并发安全，因此维护一个 channel 池，
// 每个 Publish 独占一个 channel，避免加锁串行化。
//
// 连接断开后由 supervise 后台重连：缺少这段自愈逻辑时，broker 抖动一次就会让
// 之后所有 Publish 永久失败，而调用方大多只 Warn 甚至忽略错误，事件等于静默丢失。
type Producer struct {
	cfg config.RabbitMQConfig
	log *slog.Logger

	mu   sync.RWMutex
	conn *amqp.Connection
	pool chan *amqp.Channel

	closing chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

// NewProducer 建立连接、声明拓扑并预热 channel 池。
func NewProducer(cfg config.RabbitMQConfig, log *slog.Logger) (*Producer, error) {
	size := cfg.ChannelPool
	if size <= 0 {
		// 零容量池会让每次 Publish 都新建 channel 再关掉，confirm 协商成本全白付
		size = 1
	}
	p := &Producer{
		cfg:     cfg,
		log:     log,
		pool:    make(chan *amqp.Channel, size),
		closing: make(chan struct{}),
	}
	if err := p.dial(); err != nil {
		return nil, err
	}
	p.wg.Add(1)
	go p.supervise()
	return p, nil
}

// dial 建立一条连接并完成拓扑声明与池预热。首次失败由调用方直接返回错误。
func (p *Producer) dial() error {
	conn, err := dialAMQP(p.cfg.URL)
	if err != nil {
		return fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("创建 RabbitMQ channel 失败: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return err
	}
	if p.cfg.PublisherConfirm {
		if err := ch.Confirm(false); err != nil {
			_ = ch.Close()
			_ = conn.Close()
			return fmt.Errorf("开启 publisher confirm 失败: %w", err)
		}
	}
	_ = ch.Close()

	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	p.warm(conn)
	p.log.Info("RabbitMQ 生产者就绪", "exchange", ExchangeName, "channels", len(p.pool))
	return nil
}

// warm 按池容量预热 channel；中途失败就停手，剩余请求走 acquire 的临时新建路径。
func (p *Producer) warm(conn *amqp.Connection) {
	for i := 0; i < cap(p.pool); i++ {
		c, err := conn.Channel()
		if err != nil {
			return
		}
		if p.cfg.PublisherConfirm {
			if err := c.Confirm(false); err != nil {
				_ = c.Close()
				return
			}
		}
		select {
		case p.pool <- c:
		default:
			_ = c.Close()
			return
		}
	}
}

// supervise 监听连接断开并重连，直到 Close 被调用。
func (p *Producer) supervise() {
	defer p.wg.Done()
	for {
		p.mu.RLock()
		conn := p.conn
		p.mu.RUnlock()
		if conn == nil {
			return
		}
		// 连接已断开时 NotifyClose 会立即送达，不会漏事件
		lost := conn.NotifyClose(make(chan *amqp.Error, 1))
		select {
		case <-p.closing:
			return
		case err := <-lost:
			p.log.Warn("RabbitMQ 连接断开，开始重连", "err", err)
		}
		p.drop()
		if !p.reconnect() {
			return
		}
	}
}

// reconnect 退避重连直到成功或收到退出信号，返回是否重连成功。
func (p *Producer) reconnect() bool {
	backoff := initialBackoff
	for {
		select {
		case <-p.closing:
			return false
		case <-time.After(backoff):
		}
		if err := p.dial(); err != nil {
			p.log.Warn("RabbitMQ 重连失败，稍后重试", "err", err, "backoff", backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		p.log.Info("RabbitMQ 生产者已重连", "exchange", ExchangeName)
		return true
	}
}

// drop 丢弃当前连接与池内 channel。旧连接上的 channel 一律不可复用。
func (p *Producer) drop() {
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

// Publish 投递事件到交换机，routing key 使用事件类型。
func (p *Producer) Publish(ctx context.Context, evt service.Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("序列化事件失败: %w", err)
	}

	ch, err := p.acquire()
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
func (p *Producer) acquire() (*amqp.Channel, error) {
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
		// 重连中：快速失败，不阻塞业务主流程；supervise 会在后台恢复
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

// Close 停止重连并关闭连接与全部 channel。
func (p *Producer) Close() {
	p.once.Do(func() { close(p.closing) })
	p.wg.Wait()
	p.drop()
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
