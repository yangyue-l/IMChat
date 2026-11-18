package service

import (
	"context"
	"time"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// 本文件是撤回链路：取消息 → 判权限与时间窗 → 置状态位 → 广播撤回帧。
// 类型定义与共享基元在 message.go；发送在 message_send.go；读路径在 message_read.go。

// Recall 撤回消息。发送者本人受时间窗约束；群管理员/群主可撤回他人消息，不受窗口限制。
func (s *MessageService) Recall(ctx context.Context, uid, msgID int64) error {
	msg, err := s.deps.Messages.GetByID(ctx, msgID)
	if err != nil {
		return resp.WithMsg(resp.ErrNotFound, "消息不存在")
	}

	// 只有「撤回他人消息」这一条路才需要知道操作者在群里的角色，
	// 正常撤回自己的消息不该多打一次 group_members 查询。
	var member *model.GroupMember
	if msg.FromID != uid && msg.ConvType == model.ConvGroup {
		member, err = s.rel.Member(ctx, msg.GroupID, uid)
		if err != nil {
			return err
		}
	}
	if err := canRecall(msg, uid, member, s.recallWindow, time.Now()); err != nil {
		return err
	}
	if msg.Status == model.MsgStatusRecalled {
		return nil
	}
	if err := s.deps.Messages.Recall(ctx, msgID); err != nil {
		return err
	}
	// 图片消息撤回后，content 里那个对象不再被任何可见内容引用，顺手回收。
	// 归属者用发送者而不是操作者：管理员撤回他人消息时两者不同，
	// 用操作者判断会让本该回收的对象全部跳过。
	if msg.MsgType == model.MsgImage {
		dropObjects(ctx, s.deps.Files, msg.FromID, msg.Content)
	}

	payload := NewFrame(FrameMessageRecall, map[string]any{
		"conv_id": msg.ConvID, "msg_id": msg.ID, "seq": msg.Seq, "operator": uid,
	}).Encode()
	if msg.ConvType == model.ConvPrivate {
		s.deps.Pusher.PushToUser(msg.ToID, payload)
		s.deps.Pusher.PushToUser(msg.FromID, payload)
	} else {
		ids, err := s.rel.MemberIDs(ctx, msg.GroupID)
		if err == nil {
			s.deps.Pusher.PushToUsers(ids, payload)
		}
	}
	return nil
}

// canRecall 判断 uid 能否撤回 msg，纯函数便于离线覆盖各条分支。
//
// member 是 uid 在 msg 所属群里的成员记录，私聊或撤回自己的消息时传 nil。
// 判定顺序有意为之：先分「自己的」与「他人的」，时间窗只作用于前者 ——
// 管理员撤回他人消息是内容治理动作，卡窗口等于让人等到违规内容沉淀之后才能处理。
func canRecall(msg *model.Message, uid int64, member *model.GroupMember, window time.Duration, now time.Time) error {
	if window <= 0 {
		window = defaultRecallWindow
	}
	if msg.FromID == uid {
		// 撤回自己的消息严格受窗口约束：超过之后对方大概率已读，
		// 随意撤回会让「发完再收回」变成抹除证据的手段。
		if now.Sub(msg.CreatedAt) > window {
			return resp.WithMsg(resp.ErrForbidden, "超过撤回时限的消息无法撤回")
		}
		return nil
	}
	// 他人消息只有管理员能动，且必须落在群会话里：私聊两头是平等的，
	// 不存在「第三方管理员」这种角色。
	if msg.ConvType != model.ConvGroup || member == nil || !member.IsAdmin() {
		return resp.WithMsg(resp.ErrForbidden, "只能撤回自己发送的消息")
	}
	return nil
}
