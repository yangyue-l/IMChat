package ws

import (
	"bytes"
	"errors"
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
