package ws

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

// 连接级参数。
const (
	maxMessageSize = 8 << 10 // 单帧上限 8KB，超出直接断开，防内存攻击
	writeWait      = 10 * time.Second
)

// Client 表示一条 WebSocket 连接。读写各一个 goroutine，关闭时只做一次清理。
type Client struct {
	uid    int64
	device string
	conn   *websocket.Conn
	hub    *Hub
	send   chan []byte
	closed chan struct{}

	closeOnce  sync.Once
	limiter    *rate.Limiter
	log        *slog.Logger
	onlineTick int // 用于降低在线状态续期频率
}

func newClient(hub *Hub, conn *websocket.Conn, uid int64, device string) *Client {
	cfg := hub.serverCfg
	return &Client{
		uid:     uid,
		device:  device,
		conn:    conn,
		hub:     hub,
		send:    make(chan []byte, cfg.WSSendBuffer),
		closed:  make(chan struct{}),
		limiter: rate.NewLimiter(rate.Limit(20), 40),
		log:     hub.log.With("uid", uid, "device", device),
	}
}

// setLimiter 用配置覆盖默认限流参数。
func (c *Client) setLimiter(qps float64, burst int) {
	if qps > 0 && burst > 0 {
		c.limiter = rate.NewLimiter(rate.Limit(qps), burst)
	}
}

// run 启动写协程并阻塞在读循环上，连接结束时统一清理。
func (c *Client) run() {
	go c.writeLoop()
	c.readLoop()

	c.Close()
	c.hub.unregister(c)
	c.markOffline()
}

// readLoop 读取上行帧。读超时由 Pong 续期，长时间无 pong 视为死连接。
func (c *Client) readLoop() {
	cfg := c.hub.serverCfg
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(cfg.WSPongWait.Get()))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(cfg.WSPongWait.Get()))
	})

	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				c.log.Debug("连接异常断开", "err", err)
			}
			return
		}
		c.hub.msgCount.Add(1)

		// 每连接令牌桶限流，挡住刷屏与恶意构造
		if !c.limiter.Allow() {
			_ = c.trySend([]byte(`{"type":"error","data":{"msg":"发送过于频繁"}}`))
			continue
		}
		if c.hub.onFrame != nil {
			c.hub.onFrame(c, payload)
		}
	}
}

// writeLoop 串行写出下行帧，并在同一 goroutine 内驱动心跳，避免额外 goroutine。
func (c *Client) writeLoop() {
	cfg := c.hub.serverCfg
	ticker := time.NewTicker(cfg.WSPingInterval.Get())
	defer ticker.Stop()

	for {
		select {
		case payload, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(cfg.WSWriteWait.Get()))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.log.Debug("写帧失败", "err", err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(cfg.WSWriteWait.Get()))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
			// 集群模式下才维护 Redis 在线状态，单机靠本地分片判断，零额外开销
			if c.hub.clusterEnabled {
				c.onlineTick++
				if c.onlineTick%2 == 0 {
					c.renewOnline()
				}
			}
		case <-c.closed:
			return
		}
	}
}

// trySend 非阻塞入队。队列满说明消费过慢，直接断开，保护服务端内存。
func (c *Client) trySend(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	case <-c.closed:
		return false
	default:
		c.log.Warn("发送队列已满，断开慢连接", "buffer", cap(c.send))
		c.Close()
		return false
	}
}

// Close 幂等关闭连接。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

// markOffline 清除在线状态。
func (c *Client) markOffline() {
	if !c.hub.clusterEnabled {
		return
	}
	ctx, cancel := c.hub.bgContext()
	defer cancel()
	c.hub.markOffline(ctx, c.uid, c.device)
}

// renewOnline 续期在线状态。多端共用同一个 Key，但过期时刻按设备独立计算，
// 因此这里只需刷新自己那一份记录。
func (c *Client) renewOnline() {
	ctx, cancel := c.hub.bgContext()
	defer cancel()
	c.hub.MarkOnline(ctx, c.uid, c.device, c.hub.serverCfg.WSPongWait.Get())
}
