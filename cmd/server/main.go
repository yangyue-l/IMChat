// Command server 启动 IM 服务：HTTP API + WebSocket 网关 + 异步事件消费。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"imchat/internal/cache"
	"imchat/internal/config"
	"imchat/internal/handler"
	"imchat/internal/infra"
	"imchat/internal/middleware"
	"imchat/internal/mq"
	"imchat/internal/pkg/jwt"
	"imchat/internal/pkg/logger"
	"imchat/internal/pkg/snowflake"
	"imchat/internal/repository"
	"imchat/internal/service"
	"imchat/internal/ws"
)

func main() {
	configPath := flag.String("c", "config/config.yaml", "配置文件路径")
	nodeID := flag.Int64("node", 1, "节点 ID，集群部署时每个实例必须不同")
	flag.Parse()

	if err := run(*configPath, *nodeID); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, nodeID int64) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := logger.Init(cfg.Server.Mode)
	if err := snowflake.Init(nodeID); err != nil {
		return err
	}
	nodeTag := "node-" + strconv.FormatInt(nodeID, 10)

	// ---------- 基础设施 ----------
	db, err := infra.NewMySQL(cfg.MySQL, cfg.Server.Mode)
	if err != nil {
		return err
	}
	if cfg.MySQL.AutoMigrate {
		if err := infra.Migrate(db); err != nil {
			return err
		}
	}
	rdb, err := infra.NewRedis(cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	store := cache.New(rdb, cfg.Cache)
	jwtMgr := jwt.New(cfg.JWT)

	// ---------- 事件总线：优先 RabbitMQ，未启用时降级为本地内存队列 ----------
	registry := mq.NewRegistry()
	var (
		publisher service.Publisher
		producer  *mq.Producer
		consumer  *mq.Consumer
		localBus  *mq.LocalBus
	)
	if cfg.RabbitMQ.Enabled {
		producer, err = mq.NewProducer(cfg.RabbitMQ, log)
		if err != nil {
			return err
		}
		publisher = producer
		consumer = mq.NewConsumer(cfg.RabbitMQ, registry, log)
	} else {
		localBus = mq.NewLocalBus(registry, 4096, log)
		publisher = localBus
	}

	// ---------- 连接中心与业务服务 ----------
	hub := ws.NewHub(cfg, rdb, store, nodeTag, log)
	svc := service.New(service.Deps{
		DB: db, Cache: store, JWT: jwtMgr, Cfg: cfg, Pusher: hub, Pub: publisher,
		Users:    repository.NewUserRepo(db),
		Friends:  repository.NewFriendRepo(db),
		Groups:   repository.NewGroupRepo(db),
		Messages: repository.NewMessageRepo(db),
		Moments:  repository.NewMomentRepo(db),
	})

	registerEventHandlers(registry, svc, log)

	hub.Start()
	if consumer != nil {
		consumer.Start(context.Background())
	} else {
		localBus.Start(2)
	}

	// ---------- HTTP ----------
	// 分布式限流后端只在开关打开时注入：把 nil *cache.Cache 赋给接口会得到一个
	// 非 nil 的接口值，判定逻辑就会走进 Redis 分支并空指针崩溃。
	var rateStore middleware.GlobalStore
	if cfg.RateLimit.Enabled && cfg.RateLimit.Distributed {
		rateStore = store
	}
	limiter := middleware.NewLimiter(cfg.RateLimit.Enabled, rateStore)
	// 清理阈值取最长窗口的两倍以上，保证桶被回收时令牌早已回满，不影响后续判定
	limiter.StartCleanup(5*time.Minute, 10*time.Minute)

	wsHandler := ws.NewHandler(hub, jwtMgr, svc, store, cfg.RateLimit, log)
	engine := handler.NewRouter(cfg, svc, jwtMgr, store, hub, wsHandler, limiter, log)
	engine.SetTrustedProxies(nil)

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout.Get(),
		WriteTimeout:      cfg.Server.WriteTimeout.Get(),
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("服务已启动", "addr", cfg.Server.Addr, "node", nodeTag,
			"mq", cfg.RabbitMQ.Enabled, "cluster", cfg.Cluster.Enabled, "pprof", cfg.Server.Pprof)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// ---------- 优雅停机 ----------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常: %w", err)
	case <-ctx.Done():
		log.Info("收到退出信号，开始优雅停机")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.GracefulTimeout.Get())
	defer cancel()

	// 顺序：先停止接收新请求 -> 断开 WebSocket -> 停止事件消费 -> 释放连接
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 优雅关闭超时", "err", err)
	}
	hub.Stop(shutdownCtx)
	if consumer != nil {
		consumer.Close()
		if producer != nil {
			producer.Close()
		}
	} else {
		localBus.Close()
	}
	log.Info("服务已退出")
	return nil
}

// registerEventHandlers 把事件类型绑定到业务处理函数。
// 同一个注册表既被 RabbitMQ 消费者使用，也被本地总线使用，两种模式行为一致。
func registerEventHandlers(registry *mq.Registry, svc *service.Services, log *slog.Logger) {
	// 朋友圈写扩散
	registry.Register(service.EventMomentFanout, func(ctx context.Context, data json.RawMessage) error {
		var p struct {
			MomentID int64 `json:"moment_id"`
			UserID   int64 `json:"user_id"`
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		return svc.Moment.FanoutMoment(ctx, p.MomentID, p.UserID)
	})

	// 离线消息通知。真实场景可在此对接 APNs / 短信 / 厂商推送
	registry.Register(service.EventOfflineNotify, func(ctx context.Context, data json.RawMessage) error {
		var p struct {
			ConvID  string `json:"conv_id"`
			FromID  int64  `json:"from_id"`
			GroupID int64  `json:"group_id"`
			Seq     int64  `json:"seq"`
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		log.Debug("离线消息待推送", "conv_id", p.ConvID, "from", p.FromID, "seq", p.Seq)
		return nil
	})
}
