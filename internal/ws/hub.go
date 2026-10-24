// Package ws 实现 WebSocket 接入层：分片连接管理、心跳保活、下行推送与跨节点广播。
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"imchat/internal/config"
)

// shard 是一个连接分片，独立加锁，避免全局锁成为瓶颈。
type shard struct {
	mu    sync.RWMutex
	conns map[int64]map[string]*Client // uid -> device -> client
}

func newShard() *shard {
	return &shard{conns: make(map[int64]map[string]*Client)}
}

// push 向该分片内某用户的所有连接投递，返回成功入队的连接数。
func (s *shard) push(uid int64, payload []byte) int {
	s.mu.RLock()
	conns := s.conns[uid]
	if len(conns) == 0 {
		s.mu.RUnlock()
		return 0
	}
	// 复制一份连接列表，避免持锁期间做耗时操作
	clients := make([]*Client, 0, len(conns))
	for _, c := range conns {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	n := 0
	for _, c := range clients {
		if c.trySend(payload) {
			n++
		}
	}
	return n
}

func (s *shard) add(c *Client) (replaced *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.conns[c.uid]
	if !ok {
		m = make(map[string]*Client, 2)
		s.conns[c.uid] = m
	}
	// 同一设备重复登录时顶掉旧连接，保证同端唯一
	if old, exist := m[c.device]; exist {
		replaced = old
	}
	m[c.device] = c
	return replaced
}

func (s *shard) remove(c *Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.conns[c.uid]
	if !ok {
		return false
	}
	// 只有当前登记的就是自己才删除，避免顶号时误删新连接
	if cur, exist := m[c.device]; !exist || cur != c {
		return false
	}
	delete(m, c.device)
	if len(m) == 0 {
		delete(s.conns, c.uid)
	}
	return true
}

func (s *shard) has(uid int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.conns[uid]) > 0
}

// accept 判断能否再接入一个连接。同设备重连总是放行，它只会顶掉旧连接，
// 不增加总数；新设备才受 maxConns 约束。
func (s *shard) accept(uid int64, device string, maxConns int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.conns[uid]
	if _, exist := m[device]; exist {
		return true
	}
	return len(m) < maxConns
}

// count 返回本分片连接总数，用于监控指标。
func (s *shard) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.conns {
		n += len(m)
	}
	return n
}

// OnlineStore 维护集群模式下「按设备」的在线状态，由 cache 包实现。
//
// 唯一要求：每个设备必须独立过期，不能用 Key 级 TTL。多端共用一个 Key 时，
// 任一设备心跳都会刷新整个 Key 的 TTL，已断开的设备被无限期保留，
// IsOnline 就会长期误报在线。
type OnlineStore interface {
	SetOnline(ctx context.Context, uid int64, device string, ttl time.Duration) error
	SetOffline(ctx context.Context, uid int64, device string) error
}

// busMessage 是跨节点广播的消息体。
type busMessage struct {
	Origin  string  `json:"origin"`
	UIDs    []int64 `json:"uids"`
	Payload []byte  `json:"payload"`
}

// Hub 管理全部 WebSocket 连接，并实现 service.Pusher 接口。
// 本地推送走分片，集群模式下额外通过 Redis Pub/Sub 广播到其他节点。
type Hub struct {
	shards    []*shard
	shardMask uint64
	serverCfg config.ServerConfig

	rdb            *redis.Client
	online         OnlineStore
	clusterEnabled bool
	busChannel     string
	nodeID         string
	busCh          chan []byte // 跨节点广播队列，避免每条消息起一个 goroutine

	onFrame func(c *Client, raw []byte) // 上行帧处理回调，由 handler 注入

	log       *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	connCount atomic.Int64
	msgCount  atomic.Int64
}

// NewHub 创建连接中心。shards 必须为 2 的幂，便于用位运算取模。
// online 仅集群模式使用，可为 nil。
func NewHub(cfg *config.Config, rdb *redis.Client, online OnlineStore, nodeID string, log *slog.Logger) *Hub {
	n := cfg.Cluster.Shards
	shards := make([]*shard, n)
	for i := range shards {
		shards[i] = newShard()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		shards:         shards,
		shardMask:      uint64(n - 1),
		serverCfg:      cfg.Server,
		rdb:            rdb,
		online:         online,
		clusterEnabled: cfg.Cluster.Enabled,
		busChannel:     cfg.Cluster.BusChannel,
		nodeID:         nodeID,
		log:            log,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// SetFrameHandler 注册上行帧处理函数。
func (h *Hub) SetFrameHandler(fn func(c *Client, raw []byte)) { h.onFrame = fn }

// Start 启动跨节点总线：固定 worker 消费广播队列，避免高并发下 goroutine 爆炸。
func (h *Hub) Start() {
	if !h.clusterEnabled {
		h.log.Info("集群模式未启用，仅支持单实例推送")
		return
	}
	h.busCh = make(chan []byte, 4096)
	for i := 0; i < busWorkers; i++ {
		h.wg.Add(1)
		go h.busWorker()
	}
	h.wg.Add(1)
	go h.subscribeBus()
	h.log.Info("已启用 Redis 跨节点总线", "channel", h.busChannel, "node", h.nodeID, "workers", busWorkers)
}

// busWorkers 是广播发布协程数，Redis 单命令耗时极低，2 个足够。
const busWorkers = 2

func (h *Hub) busWorker() {
	defer h.wg.Done()
	for {
		select {
		case <-h.ctx.Done():
			return
		case body := <-h.busCh:
			h.publishBusSync(body)
		}
	}
}

// Stop 关闭全部连接并退出订阅。
func (h *Hub) Stop(ctx context.Context) {
	h.cancel()
	h.closeAll()

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// shardOf 用 FNV 风格混合后取模，把用户均匀打散到各分片。
func (h *Hub) shardOf(uid int64) *shard {
	x := uint64(uid)
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return h.shards[x&h.shardMask]
}

// PushToUser 推送给某个用户在所有端上的连接。
func (h *Hub) PushToUser(uid int64, payload []byte) int {
	return h.PushToUsers([]int64{uid}, payload)
}

// PushToUsers 批量推送。本地按分片聚合加锁，集群模式下再广播一次总线消息。
func (h *Hub) PushToUsers(uids []int64, payload []byte) int {
	n := h.localPush(uids, payload)
	if h.clusterEnabled && len(uids) > 0 {
		h.publishBus(uids, payload)
	}
	return n
}

// localPush 只推本节点连接。按分片分组，把加锁次数从 O(用户数) 降到 O(分片数)。
func (h *Hub) localPush(uids []int64, payload []byte) int {
	if len(uids) == 0 {
		return 0
	}
	buckets := make(map[*shard][]int64, 8)
	for _, uid := range uids {
		s := h.shardOf(uid)
		buckets[s] = append(buckets[s], uid)
	}
	total := 0
	for s, ids := range buckets {
		for _, uid := range ids {
			total += s.push(uid, payload)
		}
	}
	return total
}

// LocalOnline 判断用户是否连在本节点。
func (h *Hub) LocalOnline(uid int64) bool {
	return h.shardOf(uid).has(uid)
}

// CanAccept 判断是否接纳新连接，maxConns 为单用户最大在线设备数（<=0 不限制）。
//
// 只在本节点内计数：多实例部署下每个节点各自允许 maxConns 个连接，
// 精确的全局连接数需要 Redis 计数，代价是每次握手一次往返，
// 而连接数本身已被握手限流压住，这里取本地判断足够。
func (h *Hub) CanAccept(uid int64, device string, maxConns int) bool {
	if maxConns <= 0 {
		return true
	}
	return h.shardOf(uid).accept(uid, device, maxConns)
}

// bgContext 返回带超时的后台上下文，用于连接生命周期内的 Redis 操作。
func (h *Hub) bgContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// MarkOnline 登记设备在线。仅集群模式生效：单机靠本地分片判断，零额外开销。
//
// 在线状态只走 cache.SetOnline 这一条路径，Key 的类型、过期语义都由它统一负责。
// 曾经这里自己拼 Key 写 set，而握手用的是 cache 的 zset，同一个 Key 两种类型，
// 续期与下线全部 WRONGTYPE 失败，在线态过期后 IsOnline 恒为 false。
func (h *Hub) MarkOnline(ctx context.Context, uid int64, device string, ttl time.Duration) {
	if !h.clusterEnabled || h.online == nil {
		return
	}
	if err := h.online.SetOnline(ctx, uid, device, ttl); err != nil {
		h.log.Debug("登记在线状态失败", "uid", uid, "device", device, "err", err)
	}
}

// markOffline 清除某个设备的在线记录，由连接退出时调用。
func (h *Hub) markOffline(ctx context.Context, uid int64, device string) {
	if !h.clusterEnabled || h.online == nil {
		return
	}
	if err := h.online.SetOffline(ctx, uid, device); err != nil {
		h.log.Debug("清除在线状态失败", "uid", uid, "device", device, "err", err)
	}
}

// OnlineCount 返回本节点连接总数。
func (h *Hub) OnlineCount() int64 { return h.connCount.Load() }

// SentFrames 返回已下发帧计数，用于监控吞吐。
func (h *Hub) SentFrames() int64 { return h.msgCount.Load() }

// publishBus 把广播消息投入队列。队列满时退化为同步发送，宁可多等一次 RTT 也不丢广播。
func (h *Hub) publishBus(uids []int64, payload []byte) {
	body, err := json.Marshal(busMessage{Origin: h.nodeID, UIDs: uids, Payload: payload})
	if err != nil || h.busCh == nil {
		return
	}
	select {
	case h.busCh <- body:
	default:
		h.publishBusSync(body)
	}
}

// publishBusSync 同步发送一条总线消息，本身持有超时，不会无限阻塞。
func (h *Hub) publishBusSync(body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.rdb.Publish(ctx, h.busChannel, body).Err(); err != nil && h.ctx.Err() == nil {
		h.log.Warn("跨节点广播失败", "err", err)
	}
}

// subscribeBus 订阅跨节点广播，跳过自身发出的消息避免重复推送。
func (h *Hub) subscribeBus() {
	defer h.wg.Done()
	sub := h.rdb.Subscribe(h.ctx, h.busChannel)
	defer func() { _ = sub.Close() }()

	for {
		select {
		case <-h.ctx.Done():
			return
		case msg, ok := <-sub.Channel():
			if !ok {
				return
			}
			var bm busMessage
			if err := json.Unmarshal([]byte(msg.Payload), &bm); err != nil {
				continue
			}
			if bm.Origin == h.nodeID {
				continue
			}
			h.localPush(bm.UIDs, bm.Payload)
		}
	}
}

// register 把连接登记到分片，返回被顶替的旧连接。
func (h *Hub) register(c *Client) *Client {
	old := h.shardOf(c.uid).add(c)
	if old == nil {
		h.connCount.Add(1)
	}
	return old
}

// unregister 注销连接。
func (h *Hub) unregister(c *Client) {
	if h.shardOf(c.uid).remove(c) {
		h.connCount.Add(-1)
	}
}

// closeAll 优雅关闭：通知所有连接下线。
func (h *Hub) closeAll() {
	frame := []byte(`{"type":"kick","data":{"reason":"server_shutdown"}}`)
	for _, s := range h.shards {
		s.mu.RLock()
		clients := make([]*Client, 0, 64)
		for _, m := range s.conns {
			for _, c := range m {
				clients = append(clients, c)
			}
		}
		s.mu.RUnlock()
		for _, c := range clients {
			_ = c.trySend(frame)
			c.Close()
		}
	}
}
