package mq

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"imchat/internal/config"
)

// TestNewProducerFailsFast 验证不可达 broker 时 NewProducer 快速失败。
//
// 回归背景：amqp.Dial 默认没有超时，broker 半死不活时一次建连能挂几十秒，
// 既拖住启动也拖住优雅停机。拨号超时必须由 dialAMQP 统一兜住。
func TestNewProducerFailsFast(t *testing.T) {
	// 1 端口不会有人监听，TCP 直接 RST，不会长时间等待
	cfg := config.RabbitMQConfig{URL: "amqp://guest:guest@127.0.0.1:1/", ChannelPool: 1}

	start := time.Now()
	p, err := NewProducer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	elapsed := time.Since(start)

	if err == nil {
		p.Close()
		t.Fatal("不可达地址应当返回错误")
	}
	// 给一点余量，只要求不被无限期挂住
	if elapsed > 2*dialTimeout {
		t.Fatalf("建连耗时 %v，疑似未受 dialTimeout 约束", elapsed)
	}
}
