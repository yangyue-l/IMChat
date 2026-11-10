package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/cache"
	"imchat/internal/config"
	"imchat/internal/ws"
)

// proberFunc 让普通函数直接当探活替身用。
type proberFunc func(ctx context.Context) error

func (f proberFunc) Ping(ctx context.Context) error { return f(ctx) }

// newHealthRouter 构造一个只装配了探活依赖的 Router。
//
// hub 只被读一个原子计数，给空的 Hub 即可 —— 不必拉起 MySQL/Redis 或整条
// 路由表，健康检查的分支才能在本机（两者常年离线）真跑起来。
// 日志丢给 io.Discard：降级用例会打 Warn，留着会混进 go test 输出。
func newHealthRouter(probes map[string]Prober) *Router {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Cluster: config.ClusterConfig{Shards: 2}}
	return &Router{probes: probes, log: log, hub: ws.NewHub(cfg, nil, nil, "test", log)}
}

// callHealthz 直接调用处理函数，返回 HTTP 状态码与解析后的响应体。
func callHealthz(t *testing.T, r *Router) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)

	r.healthz(c)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v，原文 %s", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestHealthzReportsEachDependency 覆盖依赖组合下的整体状态与逐项结果。
func TestHealthzReportsEachDependency(t *testing.T) {
	boom := errors.New("connection refused")
	cases := []struct {
		name       string
		mysqlErr   error
		redisErr   error
		wantStatus string
		wantMySQL  string
		wantRedis  string
	}{
		{"两边都通", nil, nil, "ok", "ok", "ok"},
		{"只有 MySQL 挂", boom, nil, "degraded", "down", "ok"},
		{"只有 Redis 挂", nil, boom, "degraded", "ok", "down"},
		{"两边都挂", boom, boom, "degraded", "down", "down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHealthRouter(map[string]Prober{
				"mysql": proberFunc(func(context.Context) error { return c.mysqlErr }),
				"redis": proberFunc(func(context.Context) error { return c.redisErr }),
			})

			code, body := callHealthz(t, r)
			// 降级时 HTTP 仍是 200：状态码翻成 503 会让负载均衡直接摘机，
			// 「DB 慢一秒」就变成整个实例下线。
			if code != http.StatusOK {
				t.Fatalf("期望 HTTP 200，实际 %d", code)
			}
			if got := body["status"]; got != c.wantStatus {
				t.Fatalf("status 得到 %v，期望 %s", got, c.wantStatus)
			}
			if got := body["mysql"]; got != c.wantMySQL {
				t.Fatalf("mysql 得到 %v，期望 %s", got, c.wantMySQL)
			}
			if got := body["redis"]; got != c.wantRedis {
				t.Fatalf("redis 得到 %v，期望 %s", got, c.wantRedis)
			}
			// 原有的运维字段不能因为改探活就掉：排障时它们比 status 更有用。
			for _, key := range []string{"online", "goroutine", "time"} {
				if _, ok := body[key]; !ok {
					t.Fatalf("响应缺少字段 %s", key)
				}
			}
		})
	}
}

// TestHealthzHidesDependencyErrorDetail 锁住「不回显内部错误」。
//
// healthz 不鉴权，驱动错误里常带 DSN、主机端口与账号；只报 down 就够了，
// 具体原因看服务端日志。
func TestHealthzHidesDependencyErrorDetail(t *testing.T) {
	secret := errors.New(`dial tcp 127.0.0.1:13306: password p@ssw0rd refused`)
	r := newHealthRouter(map[string]Prober{
		"mysql": proberFunc(func(context.Context) error { return secret }),
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.healthz(c)

	raw := rec.Body.String()
	for _, fragment := range []string{"13306", "p@ssw0rd", "dial tcp"} {
		if strings.Contains(raw, fragment) {
			t.Fatalf("响应体泄露了内部错误片段 %q：%s", fragment, raw)
		}
	}
}

// TestHealthzProbeTimeoutTurnsDown 验证依赖挂住时按 probeTimeout 判降级，
// 而不是陪着它一起卡死（探活没响应与依赖不可用是两件事）。
func TestHealthzProbeTimeoutTurnsDown(t *testing.T) {
	hanging := proberFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	r := newHealthRouter(map[string]Prober{
		"mysql": hanging,
		"redis": proberFunc(func(context.Context) error { return nil }),
	})

	start := time.Now()
	code, body := callHealthz(t, r)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("期望 HTTP 200，实际 %d", code)
	}
	if body["status"] != "degraded" || body["mysql"] != "down" {
		t.Fatalf("挂住的依赖应判为 down，实际 %v", body)
	}
	// 两个依赖并发探测，总耗时应在 1 个超时量级；串行实现这里会翻倍。
	if elapsed > 2*probeTimeout {
		t.Fatalf("探活耗时 %v，超过两个超时预算", elapsed)
	}
}

// TestProbesRunConcurrently 用互相等待的探针证明并发。
//
// 串行实现里第一个探针永远等不到第二个启动，只能在超时后返错；并发则两边
// 都能在超时内互相看到。用「互等」而不是比耗时，避免在慢机器上误报。
func TestProbesRunConcurrently(t *testing.T) {
	waitFor := func(mine, peer chan struct{}) Prober {
		return proberFunc(func(ctx context.Context) error {
			close(mine)
			select {
			case <-peer:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	a, b := make(chan struct{}), make(chan struct{})
	r := newHealthRouter(map[string]Prober{
		"mysql": waitFor(a, b),
		"redis": waitFor(b, a),
	})

	_, body := callHealthz(t, r)
	if body["status"] != "ok" {
		t.Fatalf("两个探针应并发完成，实际 %v", body)
	}
}

// TestProbesReportUninitialized 验证依赖缺失时报错而不是假装健康。
func TestProbesReportUninitialized(t *testing.T) {
	ctx := context.Background()
	if err := NewSQLProbe(nil).Ping(ctx); err == nil {
		t.Fatal("连接池为 nil 时应报错")
	}
	if err := NewRedisProbe(nil).Ping(ctx); err == nil {
		t.Fatal("cache 为 nil 时应报错")
	}
	// cache 存在但底层客户端没建起来（Redis 未初始化）也要报错，
	// 否则 nil 客户端会在这里空指针，把一次探活变成 500。
	if err := NewRedisProbe(cache.New(nil, config.CacheConfig{})).Ping(ctx); err == nil {
		t.Fatal("底层客户端为 nil 时应报错")
	}
}
