package middleware

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/cache"
	"imchat/internal/pkg/jwt"
	"imchat/internal/pkg/resp"
)

// Auth 校验 JWT 并把用户信息写入上下文。登出的令牌通过 Redis 黑名单拦截。
func Auth(jwtMgr *jwt.Manager, c *cache.Cache) gin.HandlerFunc {
	return func(gc *gin.Context) {
		token := extractToken(gc)
		if token == "" {
			resp.Abort(gc, resp.ErrUnauthorized)
			return
		}
		claims, err := jwtMgr.Parse(token)
		if err != nil {
			resp.Abort(gc, err)
			return
		}
		if denied, err := c.IsTokenDenied(gc.Request.Context(), token); err != nil {
			// Redis 异常时不阻断请求，安全性优先由令牌本身保证
		} else if denied {
			resp.Abort(gc, resp.ErrUnauthorized)
			return
		}

		gc.Set(UIDKey, claims.UserID)
		gc.Set(DeviceKey, claims.Device)
		gc.Set(TokenKey, token)
		gc.Next()
	}
}

// UID 从上下文取当前用户 ID，未登录返回 0。
func UID(c *gin.Context) int64 {
	v, ok := c.Get(UIDKey)
	if !ok {
		return 0
	}
	id, _ := v.(int64)
	return id
}

// Token 从上下文取原始令牌。
func Token(c *gin.Context) string {
	return c.GetString(TokenKey)
}

// Device 从上下文取当前令牌绑定的设备标识。
// 登录时未指定设备的令牌这里为空串，调用方需要自己决定怎么展示。
func Device(c *gin.Context) string {
	return c.GetString(DeviceKey)
}

func extractToken(c *gin.Context) string {
	if auth := c.GetHeader("Authorization"); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
		return strings.TrimSpace(auth)
	}
	return c.Query("token")
}

// contextWithTimeout 供 Timeout 中间件使用。
func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
