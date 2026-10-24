// Package middleware 提供鉴权、限流、日志、跨域与异常兜底。
package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/pkg/resp"
)

// UIDKey 是上下文里存放用户 ID 的键。
const UIDKey = "uid"

// DeviceKey 是上下文里存放设备标识的键。
const DeviceKey = "device"

// TokenKey 是上下文里存放原始令牌的键，登出时使用。
const TokenKey = "token"

// AccessLog 记录访问日志，只输出必要字段并跳过健康检查，避免日志洪峰。
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.Request.URL.Path
		if path == "/healthz" || path == "/metrics" {
			return
		}
		cost := time.Since(start)
		// 慢请求单独标记，便于定位性能问题
		level := slog.LevelDebug
		if cost > 500*time.Millisecond {
			level = slog.LevelWarn
		}
		log.Log(c.Request.Context(), level, "http",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"cost_ms", cost.Milliseconds(),
			"ip", c.ClientIP(),
			"uid", c.GetInt64(UIDKey),
		)
	}
}

// Recovery 捕获 panic，转成统一错误响应并记录堆栈。
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("接口 panic", "path", c.Request.URL.Path, "panic", r)
				resp.Abort(c, resp.WithMsg(resp.ErrInternal, "服务器内部错误"))
			}
		}()
		c.Next()
	}
}

// CORS 允许本地前端联调；生产环境应改为白名单。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Requested-With")
		h.Set("Access-Control-Max-Age", "86400")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// Timeout 给每个请求设置超时上下文，避免慢查询拖垮连接池。
func Timeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := contextWithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
