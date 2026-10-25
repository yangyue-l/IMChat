//go:build mqsmoke

// 需要真实 broker 的冒烟用例，默认不参与 `go test ./...`：
//
//	docker compose -f deploy/docker-compose.yml up -d rabbitmq
//	go test -tags mqsmoke -v -count=1 ./internal/mq
//
// 覆盖的是「编译与单测证明不了、只能连真 broker 才看得见」的四条路径：
// 投递→多 worker 消费、失败重试计数、超限进死信、连接断开后的自愈。
package mq

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"imchat/internal/config"
	"imchat/internal/service"
)

// errSmokeFail 是恒失败的处理器返回值，用来触发重试链路。
var errSmokeFail = errors.New("冒烟用例：处理器恒失败")

func smokeCfg() config.RabbitMQConfig {
	url := os.Getenv("IMCHAT_RABBITMQ_URL")
	if url == "" {
		url = "amqp://imchat:imchat123456@127.0.0.1:5672/"
	}
	return config.RabbitMQConfig{
		URL:              url,
		Enabled:          true,
		Prefetch:         10,
		PublisherConfirm: true,
		ChannelPool:      2,
		ConsumerWorkers:  2,
	}
}

func smokeLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// purge 清空队列，避免上一轮的残留干扰计数。必须在消费者启动前调用。
func purge(t *testing.T, cfg config.RabbitMQConfig, queues ...string) {
	t.Helper()
	conn, err := dialAMQP(cfg.URL)
	if err != nil {
		t.Fatalf("清空队列时连接失败: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("清空队列时建 channel 失败: %v", err)
	}
	defer func() { _ = ch.Close() }()
	for _, q := range queues {
		if _, err := ch.QueuePurge(q, false); err != nil {
			t.Fatalf("清空 %s 失败: %v", q, err)
		}
	}
}

// TestSmokeFanout 投递 n 条，验证多 worker 全部消费并 ACK。
func TestSmokeFanout(t *testing.T) {
	cfg := smokeCfg()
	log := smokeLog()

	p, err := NewProducer(cfg, log)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	defer p.Close()

	const eventType = "smoke.fanout"
	purge(t, cfg, QueueName, DLQName)

	var handled atomic.Int64
	registry := NewRegistry()
	registry.Register(eventType, func(context.Context, json.RawMessage) error {
		handled.Add(1)
		return nil
	})

	c := NewConsumer(cfg, registry, log)
	c.Start(context.Background())
	defer c.Close()

	const total = 40
	body, err := json.Marshal(map[string]int{"v": 1})
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	for i := 0; i < total; i++ {
		if err := p.Publish(context.Background(), service.Event{Type: eventType, Data: body}); err != nil {
			t.Fatalf("第 %d 条投递失败: %v", i, err)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for handled.Load() < total && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := handled.Load(); got != total {
		t.Fatalf("期望消费 %d 条，实际 %d", total, got)
	}
	t.Logf("多 worker 消费正常：%d/%d", handled.Load(), total)
}

// TestSmokeRetryToDLQ 处理器恒失败，验证重投 3 次后带着 x-retry=2 落进死信队列。
func TestSmokeRetryToDLQ(t *testing.T) {
	cfg := smokeCfg()
	log := smokeLog()

	p, err := NewProducer(cfg, log)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	defer p.Close()

	const eventType = "smoke.fail"
	purge(t, cfg, QueueName, DLQName)

	var attempts atomic.Int64
	registry := NewRegistry()
	registry.Register(eventType, func(context.Context, json.RawMessage) error {
		attempts.Add(1)
		return errSmokeFail
	})

	c := NewConsumer(cfg, registry, log)
	c.Start(context.Background())
	defer c.Close()

	if err := p.Publish(context.Background(), service.Event{
		Type: eventType, Data: json.RawMessage(`{"v":1}`),
	}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	// 等它走完 3 次尝试
	deadline := time.Now().Add(15 * time.Second)
	for attempts.Load() < maxRetry && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := attempts.Load(); got != maxRetry {
		t.Fatalf("期望尝试 %d 次，实际 %d", maxRetry, got)
	}

	// 从死信队列里把那条消息捞出来，核对重试计数
	conn, err := dialAMQP(cfg.URL)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("建 channel 失败: %v", err)
	}
	defer func() { _ = ch.Close() }()

	var dead amqp.Delivery
	var ok bool
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, found, err := ch.Get(DLQName, true)
		if err != nil {
			t.Fatalf("读取死信队列失败: %v", err)
		}
		if found {
			dead, ok = msg, true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		t.Fatal("死信队列里没有消息，重试上限没生效")
	}
	if got := retryOf(dead.Headers); got != maxRetry-1 {
		t.Fatalf("死信里的 x-retry 期望 %d，实际 %d", maxRetry-1, got)
	}
	t.Logf("重试到死信正常：尝试 %d 次，死信 x-retry=%d", attempts.Load(), retryOf(dead.Headers))
}

// TestSmokeProducerReconnect 直接掐掉底层连接，验证生产者能自愈。
//
// 这是本次修复的核心：修复前 conn 一旦关闭，之后所有 Publish 永久失败。
func TestSmokeProducerReconnect(t *testing.T) {
	cfg := smokeCfg()
	log := smokeLog()

	p, err := NewProducer(cfg, log)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	defer p.Close()

	p.mu.RLock()
	conn := p.conn
	p.mu.RUnlock()
	if conn == nil {
		t.Fatal("生产者没有建立连接")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("关闭底层连接失败: %v", err)
	}

	// 重连窗口内 Publish 必须快速失败，不能把调用方挂住
	start := time.Now()
	probeErr := p.Publish(context.Background(), service.Event{
		Type: "smoke.reconnect", Data: json.RawMessage(`{}`),
	})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("重连窗口内 Publish 耗时 %v，应当快速失败", elapsed)
	}
	t.Logf("重连窗口内 Publish 快速返回：err=%v", probeErr)

	// 等 supervise 把连接重建起来
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.RLock()
		alive := p.conn != nil && !p.conn.IsClosed()
		p.mu.RUnlock()
		if alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.mu.RLock()
	alive := p.conn != nil && !p.conn.IsClosed()
	p.mu.RUnlock()
	if !alive {
		t.Fatal("20s 内没有重连成功")
	}

	// 自愈后必须能正常投递
	if err := p.Publish(context.Background(), service.Event{
		Type: "smoke.reconnect", Data: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("重连后投递仍然失败: %v", err)
	}
	t.Log("断线自愈正常：连接已重建且投递成功")
}
