// Package handler 提供 HTTP 接口层。所有处理函数只做参数解析与调用 service，
// 业务规则一律留在 service 内，方便复用与单测。
// 请求体的归属见 dto.go：承载业务校验的入参定义在 service，纯传输层结构定义在本包。
package handler

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/cache"
	"imchat/internal/config"
	"imchat/internal/middleware"
	"imchat/internal/pkg/jwt"
	"imchat/internal/service"
	"imchat/internal/ws"
)

// Router 汇总依赖并构建 gin 引擎。
type Router struct {
	cfg     *config.Config
	svc     *service.Services
	jwt     *jwt.Manager
	cache   *cache.Cache
	hub     *ws.Hub
	wsH     *ws.Handler
	log     *slog.Logger
	limiter *middleware.Limiter
}

// NewRouter 创建路由。
func NewRouter(cfg *config.Config, svc *service.Services, jwtMgr *jwt.Manager,
	c *cache.Cache, hub *ws.Hub, wsH *ws.Handler, limiter *middleware.Limiter, log *slog.Logger) *gin.Engine {
	r := &Router{cfg: cfg, svc: svc, jwt: jwtMgr, cache: c, hub: hub, wsH: wsH, log: log, limiter: limiter}

	gin.SetMode(gin.ReleaseMode)
	if cfg.Server.Mode == "debug" {
		gin.SetMode(gin.DebugMode)
	}

	engine := gin.New()
	engine.Use(middleware.Recovery(log), middleware.AccessLog(log), middleware.CORS())
	engine.Use(middleware.Timeout(cfg.Server.WriteTimeout.Get()))

	// 健康检查与运维接口，不参与限流
	engine.GET("/healthz", r.healthz)
	engine.GET("/metrics", r.metrics)

	// WebSocket 握手：鉴权在 handler 内完成（浏览器 WS 无法设置自定义头）。
	// 握手本身是连接数放大器，按 IP 限流挡住反复建连。
	engine.GET("/ws", r.limiter.ByIP(r.ruleWSHandshake()), r.wsH.Serve)

	// 本地文件访问，头像与朋友圈图片
	engine.Static("/uploads", cfg.Upload.Dir)

	// 演示前端
	engine.StaticFile("/", "./web/index.html")

	if cfg.Server.Pprof {
		g := engine.Group("/debug/pprof")
		g.GET("/", gin.WrapF(pprof.Index))
		g.GET("/cmdline", gin.WrapF(pprof.Cmdline))
		g.GET("/profile", gin.WrapF(pprof.Profile))
		g.GET("/symbol", gin.WrapF(pprof.Symbol))
		g.POST("/symbol", gin.WrapF(pprof.Symbol))
		g.GET("/trace", gin.WrapF(pprof.Trace))
		g.GET("/heap", gin.WrapH(pprof.Handler("heap")))
		g.GET("/goroutine", gin.WrapH(pprof.Handler("goroutine")))
	}

	api := engine.Group("/api/v1")
	{
		// 公开接口：这条路径上还没有身份，只能按 IP 限流。
		// service 层的失败锁定是按账号的，换个用户名就能绕过，撞库的真正兜底在 IP。
		// 走 Redis 滑动窗口，多节点共享同一份配额。
		pub := api.Group("")
		pub.Use(r.limiter.ByIP(r.ruleAuth()))
		{
			pub.POST("/auth/register", r.register)
			pub.POST("/auth/login", r.login)
		}

		// 读接口：进程内令牌桶，高频、无副作用，不值得为它加一次 Redis 往返
		read := api.Group("")
		read.Use(middleware.Auth(jwtMgr, c), r.limiter.ByUID(r.ruleRead()))
		{
			read.GET("/users/me", r.me)
			read.GET("/users/search", r.searchUsers)
			read.GET("/users/:user_id", r.getUser)
			read.GET("/users/:user_id/online", r.userOnline)

			read.GET("/sessions", r.listSessions)

			read.GET("/friends", r.listFriends)
			read.GET("/friend-requests", r.listFriendRequests)
			read.GET("/blacklist", r.listBlacklist)

			read.GET("/groups", r.listGroups)
			read.GET("/groups/:group_id", r.groupDetail)
			read.GET("/groups/:group_id/members", r.listGroupMembers)

			read.GET("/conversations", r.listConversations)
			read.GET("/conversations/unread", r.unreadSummary)
			read.GET("/messages", r.history)

			read.GET("/moments/timeline", r.momentTimeline)
			read.GET("/moments/users/:user_id", r.userMoments)
			read.GET("/moments/:moment_id/comments", r.listComments)
		}

		// 写接口：独立且更严的档位。发消息、发动态、上传是真正会被放大的写路径，
		// 与读共用一个桶会让刷屏额度白白吃掉正常读请求的配额。
		write := api.Group("")
		write.Use(middleware.Auth(jwtMgr, c), r.limiter.ByUID(r.ruleWrite()))
		{
			write.POST("/auth/logout", r.logout)
			write.PATCH("/users/me", r.updateProfile)
			write.DELETE("/sessions/:device", r.kickDevice)

			write.DELETE("/friends/:friend_id", r.deleteFriend)
			write.PATCH("/friends/:friend_id", r.updateFriend)
			write.POST("/friend-requests", r.sendFriendRequest)
			write.POST("/friend-requests/:request_id/handle", r.handleFriendRequest)
			write.POST("/blacklist", r.blockUser)
			write.DELETE("/blacklist/:user_id", r.unblockUser)

			write.POST("/groups", r.createGroup)
			write.PATCH("/groups/:group_id", r.updateGroup)
			write.POST("/groups/:group_id/members", r.addGroupMembers)
			write.DELETE("/groups/:group_id/members/:user_id", r.removeGroupMember)
			write.POST("/groups/:group_id/mute", r.muteGroupMember)
			write.POST("/groups/:group_id/admin", r.setGroupAdmin)
			write.POST("/groups/:group_id/transfer", r.transferGroupOwner)
			write.DELETE("/groups/:group_id", r.dissolveGroup)

			write.PATCH("/conversations/:conv_id", r.updateConversation)
			write.POST("/messages/private", r.sendPrivate)
			write.POST("/messages/group", r.sendGroup)
			write.POST("/messages/read", r.markRead)
			write.POST("/messages/:message_id/recall", r.recall)

			write.POST("/moments", r.publishMoment)
			write.DELETE("/moments/:moment_id", r.deleteMoment)
			write.POST("/moments/:moment_id/like", r.likeMoment)
			write.DELETE("/moments/:moment_id/like", r.unlikeMoment)
			write.POST("/moments/:moment_id/comments", r.addComment)
			write.DELETE("/moments/comments/:comment_id", r.deleteComment)

			// 上传最贵：一次请求要落磁盘。除了写档，再叠一档单用户上传配额
			write.POST("/upload", r.limiter.ByUID(r.ruleUpload()), r.upload)
		}
	}

	engine.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"code": 40400, "msg": "接口不存在"})
	})
	return engine
}

// 限流规则集中在这里，路由只引用规则名，避免 qps 散落在注册代码里。
// Global/Window 只在分布式开关打开时生效，否则退化为本地令牌桶。

func (r *Router) ruleRead() middleware.Rule {
	rl := r.cfg.RateLimit
	return middleware.Rule{Name: "read", QPS: rl.HTTPQPS, Burst: rl.HTTPBurst}
}

func (r *Router) ruleWrite() middleware.Rule {
	rl := r.cfg.RateLimit
	return middleware.Rule{
		Name: "write", QPS: rl.WriteQPS, Burst: rl.WriteBurst,
		Global: rl.WriteLimit, Window: rl.WriteWindow.Get(),
	}
}

func (r *Router) ruleAuth() middleware.Rule {
	rl := r.cfg.RateLimit
	return middleware.Rule{
		Name: "auth", QPS: rl.AuthQPS, Burst: rl.AuthBurst,
		Global: rl.AuthLimit, Window: rl.AuthWindow.Get(),
	}
}

func (r *Router) ruleUpload() middleware.Rule {
	rl := r.cfg.RateLimit
	return middleware.Rule{Name: "upload", QPS: rl.UploadQPS, Burst: rl.UploadBurst}
}

func (r *Router) ruleWSHandshake() middleware.Rule {
	rl := r.cfg.RateLimit
	return middleware.Rule{Name: "ws", QPS: rl.WSHandshakeQPS, Burst: rl.WSHandshakeBurst}
}

// healthz 供负载均衡探活，只做轻量判断。
func (r *Router) healthz(c *gin.Context) {
	ctx := c.Request.Context()
	status := "ok"
	if err := r.cache.Client().Ping(ctx).Err(); err != nil {
		status = "redis_unavailable"
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    status,
		"online":    r.hub.OnlineCount(),
		"goroutine": runtime.NumGoroutine(),
		"time":      time.Now().Format(time.RFC3339),
	})
}

// metrics 输出 Prometheus 文本格式的核心指标，无需引入额外依赖。
func (r *Router) metrics(c *gin.Context) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	c.Header("Content-Type", "text/plain; version=0.0.4")
	writeMetric(c, "imchat_online_connections", "当前在线连接数", float64(r.hub.OnlineCount()))
	writeMetric(c, "imchat_frames_received_total", "累计接收的上行帧数", float64(r.hub.RecvFrames()))
	writeMetric(c, "imchat_frames_sent_total", "累计写出的下行帧数", float64(r.hub.SentFrames()))
	writeMetric(c, "imchat_goroutines", "goroutine 数量", float64(runtime.NumGoroutine()))
	writeMetric(c, "imchat_heap_alloc_bytes", "堆内存占用", float64(m.HeapAlloc))
	writeMetric(c, "imchat_gc_count_total", "GC 次数", float64(m.NumGC))
}

func writeMetric(c *gin.Context, name, help string, value float64) {
	c.Writer.WriteString("# HELP " + name + " " + help + "\n")
	c.Writer.WriteString("# TYPE " + name + " gauge\n")
	c.Writer.WriteString(name + " " + strconv.FormatFloat(value, 'g', -1, 64) + "\n")
}
