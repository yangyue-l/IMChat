package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/cache"
)

// probeTimeout 是单项依赖的探测上限。
//
// 探活本身必须有界：依赖挂住时若跟着连接池的默认超时走，负载均衡先超时摘机，
// 看到的却是「healthz 没有响应」而不是「MySQL 不可用」，排障方向直接跑偏。
const probeTimeout = time.Second

// Prober 是探活需要的最小能力：判断一个外部依赖当前是否可用。
//
// 收接口而不是 *gorm.DB / *redis.Client：接口层除了探活不碰存储，收具体类型
// 会把 ORM 与客户端拖进来；更实际的原因是本机 MySQL/Redis 常年离线，
// 不抽象成接口就没法把「两边都通」「只有一边通」「探活超时」这几条分支测出来。
type Prober interface {
	Ping(ctx context.Context) error
}

// NewSQLProbe 用数据库连接池探活。
//
// 收 *sql.DB 而不是 *gorm.DB：gorm 的连接池没有 Ping，要拿还得再调一次 DB()，
// 那层转换放在装配处（cmd/server）比让接口层依赖 ORM 干净。
func NewSQLProbe(db *sql.DB) Prober { return sqlProbe{db: db} }

type sqlProbe struct{ db *sql.DB }

func (p sqlProbe) Ping(ctx context.Context) error {
	if p.db == nil {
		return errors.New("mysql 连接池未初始化")
	}
	return p.db.PingContext(ctx)
}

// NewRedisProbe 用 Redis 的 PING 命令探活。
func NewRedisProbe(c *cache.Cache) Prober { return redisProbe{c: c} }

type redisProbe struct{ c *cache.Cache }

func (p redisProbe) Ping(ctx context.Context) error {
	if p.c == nil || p.c.Client() == nil {
		return errors.New("redis 客户端未初始化")
	}
	return p.c.Client().Ping(ctx).Err()
}

// healthz 供负载均衡探活。
//
// HTTP 状态恒为 200，降级只体现在 status 字段：依赖抖一秒就把实例摘掉，
// 等于把「MySQL 慢一秒」放大成整个集群反复摘除又加回。摘不摘、摘多久，
// 交给调用方按 status 自行判断。
func (r *Router) healthz(c *gin.Context) {
	status, deps := r.probeAll(c.Request.Context())

	body := gin.H{
		"status":    status,
		"online":    r.hub.OnlineCount(),
		"goroutine": runtime.NumGoroutine(),
		"time":      time.Now().Format(time.RFC3339),
	}
	// 依赖名直接当响应字段名：加一个依赖只改装配处的表，这里不用动。
	for name, state := range deps {
		body[name] = state
	}
	c.JSON(http.StatusOK, body)
}

// probeAll 并发探测所有依赖，返回整体状态与逐项结果。
//
// 并发而不是串行：串行时两个依赖各卡满超时就翻倍成 2s，超过多数负载均衡的
// 健康检查超时，排查时和「进程卡死」分不开。
func (r *Router) probeAll(ctx context.Context) (string, map[string]string) {
	states := make(map[string]string, len(r.probes))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		degraded bool
	)
	for name, p := range r.probes {
		wg.Add(1)
		go func(name string, p Prober) {
			defer wg.Done()

			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()

			state := "ok"
			if err := p.Ping(pctx); err != nil {
				state = "down"
				// 只记日志、不回给客户端：healthz 不鉴权，驱动错误里常带 DSN、
				// 主机端口和账号，写进响应等于把内网拓扑挂在公网入口上。
				r.log.Warn("健康检查失败", "dependency", name, "err", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if state != "ok" {
				degraded = true
			}
			states[name] = state
		}(name, p)
	}
	wg.Wait()

	if degraded {
		return "degraded", states
	}
	return "ok", states
}
