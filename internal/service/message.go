package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// MessageService 消息与会话业务。
//
// 实现按职责分在 message_send.go（发送/引用/幂等）、message_read.go
// （历史/检索/已读/会话列表）、message_recall.go（撤回）里，本文件只放
// 对外契约、业务约束与两条以上链路共用的转换和鉴权基元。
type MessageService struct {
	deps Deps
	rel  *relation
	// recallWindow 撤回时间窗，来自 config.message.recall_window。
	recallWindow time.Duration
}

// 业务约束。
const (
	maxContentRunes  = 5000
	maxExtraBytes    = 1024 // 与 messages.extra 的列宽一致，超长会撞 MySQL 1406
	historyPageLimit = 50
	// defaultRecallWindow 是配置缺省时的兜底值。内部构造（测试、脚本）常常不填
	// RecallWindow，取 0 会让「超窗」判定把所有撤回都放行，静默削弱约束。
	defaultRecallWindow = 2 * time.Minute
	// 引用摘要在气泡里只占一行，超过这个长度必然是省略号，存全文纯属浪费列宽。
	maxQuotePreviewRunes = 64
	// 关键词检索的边界。下限 2 个 rune 挡掉单字检索：LIKE '%a%' 会扫出整个会话，
	// 既没有检索价值又是一次全表扫描；上限 32 与 IM 里「一句话关键词」的实际形态对齐，
	// 再长的输入更适合拿去翻历史，而不是让 LIKE 去比对一长串文本。
	minKeywordRunes    = 2
	maxKeywordRunes    = 32
	defaultSearchLimit = 20
	maxSearchLimit     = 50
)

// SendReq 发送消息参数。私聊填 ToID，群聊填 GroupID，用 ConvType 区分。
type SendReq struct {
	ConvType    int8   `json:"conv_type"`
	ToID        int64  `json:"to_id,string"`
	GroupID     int64  `json:"group_id,string"`
	MsgType     int8   `json:"msg_type"`
	Content     string `json:"content"`
	Extra       string `json:"extra"`
	QuoteMsgID  int64  `json:"quote_msg_id,string"` // 引用消息 ID，0 表示不引用
	ClientMsgID string `json:"client_msg_id"`
}

// QuoteDTO 被引用消息的摘要，随消息体一起下发。
// 只带渲染引用条所需的三个字段：客户端要展示原消息全文时应重新拉历史，
// 而不是期望每条新消息都背着一份旧消息副本。
type QuoteDTO struct {
	MsgID   int64  `json:"msg_id,string"`
	FromID  int64  `json:"from_id,string"`
	Preview string `json:"preview"`
}

// MessageDTO 对外消息结构。
type MessageDTO struct {
	ID          int64             `json:"id,string"`
	ConvID      string            `json:"conv_id"`
	ConvType    int8              `json:"conv_type"`
	Seq         int64             `json:"seq"`
	FromID      int64             `json:"from_id,string"`
	ToID        int64             `json:"to_id,string"`
	GroupID     int64             `json:"group_id,string"`
	ClientMsgID string            `json:"client_msg_id"`
	MsgType     int8              `json:"msg_type"`
	Content     string            `json:"content"`
	Extra       string            `json:"extra,omitempty"`
	Quote       *QuoteDTO         `json:"quote,omitempty"`
	Status      int8              `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	FromUser    *model.PublicUser `json:"from_user,omitempty"`
}

// ConversationDTO 会话列表项。
type ConversationDTO struct {
	ConvID         string    `json:"conv_id"`
	ConvType       int8      `json:"conv_type"`
	TargetID       int64     `json:"target_id,string"`
	Name           string    `json:"name"`
	Avatar         string    `json:"avatar"`
	LastMsgID      int64     `json:"last_msg_id,string"`
	LastMsgPreview string    `json:"last_msg_preview"`
	LastMsgType    int8      `json:"last_msg_type"`
	LastMsgAt      time.Time `json:"last_msg_at"`
	Unread         int64     `json:"unread"`
	Pinned         bool      `json:"pinned"`
	Muted          bool      `json:"muted"`
	MemberCount    int       `json:"member_count,omitempty"`
}

// UnreadItem 单个会话的未读数。
type UnreadItem struct {
	ConvID string `json:"conv_id"`
	Unread int64  `json:"unread"`
}

// UnreadSummaryDTO 未读汇总。total 由服务端算好，客户端直接拿去渲染角标，
// 不必再遍历 items 自己累加。
type UnreadSummaryDTO struct {
	Total int64        `json:"total"`
	Items []UnreadItem `json:"items"`
}

// checkConvAccess 校验当前用户对会话的访问权限。
func (s *MessageService) checkConvAccess(ctx context.Context, uid int64, convID string) error {
	convType, target, ok := parseConv(convID, uid)
	if !ok {
		return resp.WithMsg(resp.ErrForbidden, "无权访问该会话")
	}
	if convType == model.ConvPrivate {
		blocked, err := s.rel.EitherBlocked(ctx, uid, target)
		if err != nil {
			return err
		}
		if blocked {
			return resp.WithMsg(resp.ErrForbidden, "无权访问该会话")
		}
		return nil
	}
	member, err := s.rel.Member(ctx, target, uid)
	if err != nil {
		return err
	}
	if member == nil {
		return resp.WithMsg(resp.ErrForbidden, "你不在该群中")
	}
	return nil
}

// parseConv 解析会话并返回类型与目标 ID。
func parseConv(convID string, self int64) (int8, int64, bool) {
	if strings.HasPrefix(convID, "g_") {
		gid, ok := GroupID(convID)
		return model.ConvGroup, gid, ok
	}
	if uid, ok := PrivateTarget(convID, self); ok {
		return model.ConvPrivate, uid, true
	}
	return 0, 0, false
}

// toDTO 是消息出站的唯一转换点：历史、检索、发送回执三条链路都过这里，
// 少一处单独拼装就少一处「某个接口忘了带 quote」的可能。
func toDTO(m *model.Message, from *model.PublicUser) *MessageDTO {
	return &MessageDTO{
		ID: m.ID, ConvID: m.ConvID, ConvType: m.ConvType, Seq: m.Seq,
		FromID: m.FromID, ToID: m.ToID, GroupID: m.GroupID,
		ClientMsgID: m.ClientMsgID, MsgType: m.MsgType, Content: m.Content,
		Extra: m.Extra, Quote: quoteOf(m.Extra), Status: m.Status,
		CreatedAt: m.CreatedAt, FromUser: from,
	}
}

// quoteOf 从 extra 里取出引用摘要。extra 不是 JSON、没有 quote 字段或 ID 非法时
// 返回 nil —— 引用功能上线前写入的老消息没有这个字段，必须照常下发。
func quoteOf(extra string) *QuoteDTO {
	if extra == "" {
		return nil
	}
	var e struct {
		Quote *QuoteDTO `json:"quote"`
	}
	if err := json.Unmarshal([]byte(extra), &e); err != nil {
		return nil
	}
	if e.Quote == nil || e.Quote.MsgID <= 0 {
		return nil
	}
	return e.Quote
}
