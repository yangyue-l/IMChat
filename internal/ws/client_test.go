package ws

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"imchat/internal/config"
)

// TestFrameCountersCountEachDirection 锁住「上下行分开计数」这条链路。
//
// 回归背景：两个方向曾共用一个 msgCount，/metrics 却把上行收帧的数
// 挂在 imchat_frames_received_total 上，下行帧没有任何统计入口 ——
// 推送量一大就分不清积压发生在读侧还是写侧。
//
// 这里走真实握手与真实读写协程，而不是直接给计数器赋值：要证明的是
// 「收帧加在 readLoop、发帧加在成功写出之后」这两处接线没接反。
func TestFrameCountersCountEachDirection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Shards = 4                                    // 单机模式：收尾路径不碰 Redis
	cfg.Server.WSWriteWait = config.Duration(2 * time.Second) // 零值会让写超时立刻过期
	cfg.Server.WSPongWait = config.Duration(5 * time.Second)  // 零值会让读超时立刻过期
	cfg.Server.WSPingInterval = config.Duration(time.Hour)    // 心跳不参与统计，测试期间别来打扰
	cfg.Server.WSSendBuffer = 16
	hub := NewHub(cfg, nil, nil, "test-node", slog.Default())

	// 只有带 ping 的帧才回一帧，用来制造下行；其余帧不回复，
	// 这样「只收不回」的场景才能验证两个计数器互不影响。
	hub.SetFrameHandler(func(c *Client, raw []byte) {
		if bytes.Contains(raw, []byte(`"ping"`)) {
			c.trySend([]byte(`{"type":"pong"}`))
		}
	})

	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := newClient(hub, conn, 42, "web")
		hub.register(c)
		close(ready)
		go c.run()
	}))
	defer srv.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer cli.Close()
	<-ready

	if err := cli.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("发送上行帧失败: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := cli.ReadMessage(); err != nil {
		t.Fatalf("读取下行帧失败: %v", err)
	}
	// 客户端能读到回帧，说明服务端确实写成功了；等计数落账（写协程异步）
	waitFrames(t, hub, 1, 1)

	// 第二帧没有回复：收帧加一，发帧不动 —— 证明两者是独立计数器，
	// 而不是同一个原子量被两个方向共用。
	if err := cli.WriteMessage(websocket.TextMessage, []byte(`{"type":"noop"}`)); err != nil {
		t.Fatalf("发送第二帧失败: %v", err)
	}
	waitFrames(t, hub, 2, 1)
}

// waitFrames 轮询等待两个计数器到达期望值，避免依赖写入与断言的先后顺序。
func waitFrames(t *testing.T, hub *Hub, recv, sent int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.RecvFrames() == recv && hub.SentFrames() == sent {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("帧计数未达期望：recv=%d(期望 %d) sent=%d(期望 %d)",
		hub.RecvFrames(), recv, hub.SentFrames(), sent)
}

// TestDroppedFramesCountsOnlyOnFullQueue 锁住「丢弃计数只记队列满」这条口径。
//
// 回归背景：trySend 的队列满分支过去只打一条 Warn，/metrics 里看不到任何痕迹 ——
// 慢客户端被断开后，收到与发出的帧数都看不出异常，线上无法判断是没人发还是发不出去。
//
// 这里刻意不启动读写协程，让发送队列保持无人消费：只有这样才能把队列灌满，
// 又不必真等一个慢客户端。连接仍走真实握手 —— 满分支会关连接，conn 为 nil 会 panic。
func TestDroppedFramesCountsOnlyOnFullQueue(t *testing.T) {
	hub := newSendTestHub(t, 1) // 队列容量 1：第二次入队必然满
	conns := openTestConns(t, 2)

	c1 := newClient(hub, conns[0], 7, "web")
	if !c1.trySend([]byte("a")) {
		t.Fatal("队列未满时应入队成功")
	}
	if got := hub.DroppedFrames(); got != 0 {
		t.Fatalf("入队成功不应计入丢弃，实际 %d", got)
	}

	if c1.trySend([]byte("b")) {
		t.Fatal("队列已满时应拒绝入队")
	}
	if got := hub.DroppedFrames(); got != 1 {
		t.Fatalf("队列满应计 1 次丢弃，实际 %d", got)
	}

	// 队列仍满且连接已被上一步关掉：这次走 closed 分支，属正常下线而非慢客户端，
	// 不能再计一次。两个分支都返回 false，只有计数器能区分，所以必须单独断言。
	if c1.trySend([]byte("c")) {
		t.Fatal("已关闭的连接不应入队成功")
	}
	if got := hub.DroppedFrames(); got != 1 {
		t.Fatalf("已关闭连接的入队不应计入丢弃，实际 %d", got)
	}

	// 第二条连接的丢弃要累加到同一个 Hub 计数上：指标是进程级视图，
	// 计数器若挂在 Client 上，连接一断就随之消失，等于事后什么都查不到。
	c2 := newClient(hub, conns[1], 8, "ios")
	_ = c2.trySend([]byte("d"))
	_ = c2.trySend([]byte("e"))
	if got := hub.DroppedFrames(); got != 2 {
		t.Fatalf("丢弃计数应跨连接累加，实际 %d", got)
	}
}

// newSendTestHub 构造单机模式、可指定发送队列长度的 Hub。
//
// 日志换成 io.Discard：满队列分支必然打 Warn，用默认 logger 会让这些
// 预期内的告警混进测试输出，真失败时反而找不到断言信息。
func newSendTestHub(t *testing.T, sendBuffer int) *Hub {
	t.Helper()
	cfg := &config.Config{}
	cfg.Cluster.Shards = 4 // 单机模式：收尾路径不碰 Redis
	cfg.Server.WSWriteWait = config.Duration(2 * time.Second)
	cfg.Server.WSPongWait = config.Duration(5 * time.Second)
	cfg.Server.WSPingInterval = config.Duration(time.Hour)
	cfg.Server.WSSendBuffer = sendBuffer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHub(cfg, nil, nil, "test-node", log)
}

// openTestConns 建 n 条真实握手连接，返回服务端侧的 conn 供测试直接构造 Client。
//
// 客户端侧连接一直保持打开：它是「服务端 conn 还有对端」的凭据，
// 提前关掉会让服务端写帧直接失败，测出来的就不是队列满了。
func openTestConns(t *testing.T, n int) []*websocket.Conn {
	t.Helper()
	connCh := make(chan *websocket.Conn, n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connCh <- conn
	}))
	t.Cleanup(srv.Close)

	clients := make([]*websocket.Conn, 0, n)
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
	})
	for i := 0; i < n; i++ {
		cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("拨号失败: %v", err)
		}
		clients = append(clients, cli)
	}

	servers := make([]*websocket.Conn, 0, n)
	for i := 0; i < n; i++ {
		servers = append(servers, <-connCh)
	}
	return servers
}

// TestKickDeviceClosesConnection 验证踢线真的断掉连接，并把关闭码带给客户端。
//
// 走真实握手而不是直接调 Client.kick：要证明的是「关掉连接之后收尾流程照常跑完」——
// 分片注销与在线态清理都挂在 run() 的退出路径上，漏了就会留下一条永远不会离线、
// 也永远收不到消息的幽灵连接。顺带锁住「不匹配的设备名一条连接都不断」，
// 那是踢错端的唯一可能来源。
func TestKickDeviceClosesConnection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cluster.Shards = 4 // 单机模式：收尾路径不碰 Redis
	cfg.Server.WSWriteWait = config.Duration(2 * time.Second)
	cfg.Server.WSPongWait = config.Duration(5 * time.Second)
	cfg.Server.WSPingInterval = config.Duration(time.Hour)
	cfg.Server.WSSendBuffer = 16
	hub := NewHub(cfg, nil, nil, "test-node", slog.Default())

	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := newClient(hub, conn, 42, "web")
		hub.register(c)
		close(ready)
		go c.run()
	}))
	defer srv.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer cli.Close()
	<-ready

	if n := hub.KickDevice(42, "ios"); n != 0 {
		t.Fatalf("设备名不匹配时不应断开任何连接，实际 %d", n)
	}
	if n := hub.KickDevice(42, "web"); n != 1 {
		t.Fatalf("期望断开 1 条连接，实际 %d", n)
	}

	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := cli.ReadMessage(); err == nil {
		t.Fatal("踢线后连接应已断开")
	} else {
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != closeCodeKicked {
			t.Fatalf("期望关闭码 %d，实际 %v", closeCodeKicked, err)
		}
	}

	// 连接断开后收尾是异步的，等分片里的记录被清掉
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !hub.LocalOnline(42) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("踢线后连接应已从分片注销")
}
