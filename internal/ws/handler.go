package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"imchat/internal/cache"
	"imchat/internal/config"
	"imchat/internal/model"
	"imchat/internal/pkg/jwt"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// Handler 负责 HTTP 升级握手与上行帧分发。
type Handler struct {
	hub      *Hub
	jwt      *jwt.Manager
	svc      *service.Services
	cache    *cache.Cache
	upgrader websocket.Upgrader
	log      *slog.Logger

	msgQPS   float64 // 单连接上行令牌桶速率
	msgBurst int
	maxConns int // 单用户最大在线设备数
}

// NewHandler 创建 WebSocket 处理器。
func NewHandler(hub *Hub, jwtMgr *jwt.Manager, svc *service.Services, c *cache.Cache,
	rl config.RateLimitConfig, log *slog.Logger) *Handler {
	h := &Handler{
		hub: hub, jwt: jwtMgr, svc: svc, cache: c, log: log,
		msgQPS: rl.WSMsgQPS, msgBurst: rl.WSMsgBurst, maxConns: rl.WSMaxConns,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 生产环境应校验 Origin 白名单；这里放开以便本地多端口调试
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	hub.SetFrameHandler(h.handleFrame)
	return h
}

// Serve 是 gin 路由入口：校验令牌 -> 升级协议 -> 登记连接 -> 交给读写协程。
func (h *Handler) Serve(c *gin.Context) {
	token := extractToken(c)
	if token == "" {
		resp.Abort(c, resp.ErrUnauthorized)
		return
	}
	claims, err := h.jwt.Parse(token)
	if err != nil {
		resp.Abort(c, err)
		return
	}
	if denied, _ := h.cache.IsTokenDenied(c.Request.Context(), token); denied {
		resp.Abort(c, resp.ErrUnauthorized)
		return
	}

	device := c.Query("device")
	if device == "" {
		device = claims.Device
	}
	if device == "" {
		device = "web"
	}
	if len(device) > 32 {
		device = device[:32]
	}

	// 单用户连接数封顶：多开一个连接就多一份上行配额，只按连接限流的话
	// 刷连接数就能变相放大配额。必须在 Upgrade 之前判定，升级后再返回 JSON 无效。
	if !h.hub.CanAccept(claims.UserID, device, h.maxConns) {
		resp.Abort(c, resp.WithMsg(resp.ErrTooManyRequests, "在线设备数已达上限，请先退出其他设备"))
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.log.Debug("WebSocket 升级失败", "err", err, "uid", claims.UserID)
		return
	}

	client := newClient(h.hub, conn, claims.UserID, device)
	client.setLimiter(h.msgQPS, h.msgBurst)

	// 同设备重复登录时顶掉旧连接，避免一端多开导致消息重复
	if old := h.hub.register(client); old != nil {
		_ = old.trySend([]byte(`{"type":"kick","data":{"reason":"账号在其他地方登录"}}`))
		old.Close()
	}

	// 在线状态由 Hub 统一登记，与心跳续期、连接下线共用同一条路径
	onlineCtx, onlineCancel := h.hub.bgContext()
	h.hub.MarkOnline(onlineCtx, claims.UserID, device, h.hub.onlineTTL())
	onlineCancel()

	h.log.Info("连接建立", "uid", claims.UserID, "device", device, "online", h.hub.OnlineCount())
	go client.run()
}

// inboundFrame 是上行帧结构。
type inboundFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// handleFrame 处理客户端上行帧。所有慢操作都在连接自己的读协程里完成，
// 不阻塞其他连接。
func (h *Handler) handleFrame(c *Client, raw []byte) {
	var f inboundFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		c.trySend(errFrame("消息格式错误"))
		return
	}

	ctx, cancel := c.hub.bgContext()
	defer cancel()

	switch f.Type {
	case "ping":
		c.trySend([]byte(`{"type":"pong"}`))

	case "send":
		var req service.SendReq
		if err := json.Unmarshal(f.Data, &req); err != nil {
			c.trySend(errFrame("消息参数错误"))
			return
		}
		var dto *service.MessageDTO
		var err error
		switch req.ConvType {
		case model.ConvPrivate:
			dto, err = h.svc.Message.SendPrivate(ctx, c.uid, req)
		case model.ConvGroup:
			dto, err = h.svc.Message.SendGroup(ctx, c.uid, req)
		default:
			c.trySend(errFrame("会话类型非法"))
			return
		}
		if err != nil {
			c.trySend(errFrame(err.Error()))
			return
		}
		// 回执只发给发送方本身，客户端据 client_msg_id 做本地消息状态更新
		c.trySend(service.NewFrame("ack", dto).Encode())

	case "read":
		var req struct {
			ConvID string `json:"conv_id"`
			Seq    int64  `json:"seq"`
		}
		if err := json.Unmarshal(f.Data, &req); err != nil {
			c.trySend(errFrame("已读参数错误"))
			return
		}
		if err := h.svc.Message.MarkRead(ctx, c.uid, req.ConvID, req.Seq); err != nil {
			c.trySend(errFrame(err.Error()))
		}

	case "typing":
		var req struct {
			ConvID string `json:"conv_id"`
		}
		if err := json.Unmarshal(f.Data, &req); err != nil {
			return
		}
		h.relayTyping(c.uid, req.ConvID)

	default:
		c.trySend(errFrame("未知的消息类型"))
	}
}

// relayTyping 转发"正在输入"提示。该信号不落库、不重试，丢了也无所谓。
func (h *Handler) relayTyping(uid int64, convID string) {
	frame := service.NewFrame("typing", map[string]any{"conv_id": convID, "from": uid}).Encode()
	if gid, ok := service.GroupID(convID); ok {
		members, err := h.svc.Message.MemberIDs(context.Background(), gid)
		if err != nil {
			return
		}
		filtered := make([]int64, 0, len(members))
		for _, m := range members {
			if m != uid {
				filtered = append(filtered, m)
			}
		}
		h.hub.PushToUsers(filtered, frame)
		return
	}
	if target, ok := service.PrivateTarget(convID, uid); ok {
		h.hub.PushToUser(target, frame)
	}
}

func errFrame(msg string) []byte {
	return service.NewFrame("error", map[string]any{"msg": msg}).Encode()
}

// extractToken 支持 Authorization 头与 query 参数两种取法（浏览器 WS 无法自定义头）。
func extractToken(c *gin.Context) string {
	if auth := c.GetHeader("Authorization"); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
		return strings.TrimSpace(auth)
	}
	return c.Query("token")
}
