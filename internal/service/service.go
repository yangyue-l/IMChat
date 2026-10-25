// Package service 承载业务逻辑。HTTP handler 与 WebSocket 网关都只调用这一层。
package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"imchat/internal/cache"
	"imchat/internal/pkg/jwt"
	"imchat/internal/repository"
)

// Pusher 是 WebSocket 推送能力的抽象。
// service 只依赖接口，不依赖 ws 包，避免双向依赖；单机与集群由 Hub 内部实现差异。
//
// 故意不暴露「本节点是否在线」：service 判定在线必须走 Presence，
// 本节点未命中不代表人不在线（集群下消息由总线送到其他节点）。
type Pusher interface {
	// PushToUser 推送给某用户的所有本节点连接，返回推送成功的连接数。
	PushToUser(uid int64, payload []byte) int
	// PushToUsers 批量推送，内部按分片聚合，避免逐用户加锁。
	PushToUsers(uids []int64, payload []byte) int
}

// Presence 是在线状态查询能力的抽象，由 ws.Hub 实现。
// 单机直接读本地连接表，集群模式本地未命中时才回源 Redis —— 差异全在实现里，
// 调用方不需要知道自己跑在哪种模式上。
type Presence interface {
	// Online 返回这批用户里在线的那些，map 中存在的 uid 即为在线。
	// 实现失败时返回 error 而不是空 map：调用方需要能区分「不在线」与「查不到」。
	Online(uids []int64) (map[int64]bool, error)
}

// Event 是投递到消息队列的异步事件。
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// 事件类型常量。新增事件必须同时在 cmd/server 里 Register 处理器，
// 只定义常量不注册等于生产者往空气里投消息。
const (
	EventMomentFanout  = "moment.fanout"   // 朋友圈动态写扩散
	EventOfflineNotify = "message.offline" // 离线消息通知
)

// Publisher 是消息队列生产者的抽象，未启用 MQ 时由本地实现兜底。
type Publisher interface {
	Publish(ctx context.Context, evt Event) error
}

// Deps 汇总 service 依赖，由 main 统一装配。
type Deps struct {
	DB       *gorm.DB
	Cache    *cache.Cache
	JWT      *jwt.Manager
	Pusher   Pusher
	Presence Presence
	Pub      Publisher

	Users    *repository.UserRepo
	Friends  *repository.FriendRepo
	Groups   *repository.GroupRepo
	Messages *repository.MessageRepo
	Moments  *repository.MomentRepo
}

// Services 是全部业务服务的聚合入口。
type Services struct {
	User    *UserService
	Friend  *FriendService
	Group   *GroupService
	Message *MessageService
	Moment  *MomentService
	rel     *relation
}

// New 组装业务服务。
func New(d Deps) *Services {
	rel := &relation{cache: d.Cache, friends: d.Friends, groups: d.Groups, users: d.Users}
	return &Services{
		User:    &UserService{deps: d, rel: rel},
		Friend:  &FriendService{deps: d, rel: rel},
		Group:   &GroupService{deps: d, rel: rel},
		Message: &MessageService{deps: d, rel: rel},
		Moment:  &MomentService{deps: d, rel: rel},
		rel:     rel,
	}
}

// PrivateConvID 生成私聊会话 ID，双方 uid 排序后拼接，保证唯一且可逆。
func PrivateConvID(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return "p_" + strconv.FormatInt(a, 10) + "_" + strconv.FormatInt(b, 10)
}

// GroupConvID 生成群聊会话 ID。
func GroupConvID(gid int64) string { return "g_" + strconv.FormatInt(gid, 10) }

// PrivateTarget 从私聊会话 ID 中解析出对方的 uid。
func PrivateTarget(convID string, self int64) (int64, bool) {
	if !strings.HasPrefix(convID, "p_") {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(convID, "p_"), "_")
	if len(parts) != 2 {
		return 0, false
	}
	a, err1 := strconv.ParseInt(parts[0], 10, 64)
	b, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	switch self {
	case a:
		return b, true
	case b:
		return a, true
	default:
		return 0, false
	}
}

// GroupID 从群会话 ID 中解析群 ID。
func GroupID(convID string) (int64, bool) {
	if !strings.HasPrefix(convID, "g_") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(convID, "g_"), 10, 64)
	return id, err == nil
}

// 推送帧类型，前端据此分发。
const (
	FrameMessage       = "message"
	FrameMessageRecall = "message_recall"
	FrameRead          = "read"
	FrameFriendRequest = "friend_request"
	FrameFriendAccept  = "friend_accept"
	FrameGroupEvent    = "group_event"
	FrameMomentNew     = "moment_new"
	FrameNotice        = "notice"
	FrameKick          = "kick"
)

// Frame 是 WebSocket 下行消息的统一结构。
type Frame struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	Ts   int64  `json:"ts"`
}

// NewFrame 构造下行帧。
func NewFrame(typ string, data any) Frame {
	return Frame{Type: typ, Data: data, Ts: time.Now().UnixMilli()}
}

// Encode 序列化帧。广播场景只序列化一次，多连接复用同一份字节，减少 CPU 与 GC 压力。
func (f Frame) Encode() []byte {
	b, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	return b
}
