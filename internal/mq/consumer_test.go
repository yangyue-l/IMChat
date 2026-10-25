package mq

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"imchat/internal/config"
)

// TestRetryOf 锁定 x-retry 的宽容解析：只有认得出整数类型才能正常计数，
// 认不出就退化成 0，等于无限重试。
func TestRetryOf(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"缺失头", nil, 0},
		{"本端生产的 int32", int32(2), 2},
		{"原生 int", 3, 3},
		{"他端常见的 int64", int64(1), 1},
		{"JSON 中转的 float64", float64(4), 4},
		{"无符号", uint32(5), 5},
		{"字符串不认", "2", 0},
		{"负数归零", int32(-3), 0},
		{"上限内不截断", int64(maxRetry - 1), maxRetry - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := amqp.Table{}
			if c.value != nil {
				h["x-retry"] = c.value
			}
			if got := retryOf(h); got != c.want {
				t.Fatalf("retryOf = %d, want %d", got, c.want)
			}
		})
	}
}

// TestConsumerWorkers 是 workers() 的防御性兜底：零值必须退化为 1，
// 否则会意外放大未确认消息总量。正常启动路径上配置已由 config.Load 兜到 3。
func TestConsumerWorkers(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.RabbitMQConfig
		want int
	}{
		{"未配置", config.RabbitMQConfig{}, 1},
		{"显式配置", config.RabbitMQConfig{ConsumerWorkers: 5}, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (&Consumer{cfg: c.cfg}).workers(); got != c.want {
				t.Fatalf("workers = %d, want %d", got, c.want)
			}
		})
	}
}
