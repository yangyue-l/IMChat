package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// errRequestHandled 表示申请已被并发请求处理掉，用于事务内部回滚并转成业务错误。
var errRequestHandled = errors.New("好友申请已被处理")

// FriendService 好友与黑名单业务。
type FriendService struct {
	deps Deps
	rel  *relation
}

// SendFriendReq 发起好友申请。
type SendFriendReq struct {
	ToUserID int64  `json:"to_user_id,string"`
	Message  string `json:"message"`
}

// FriendItem 好友列表项，包含备注与对方资料。
type FriendItem struct {
	model.PublicUser
	Remark    string    `json:"remark"`
	Starred   bool      `json:"starred"`
	CreatedAt time.Time `json:"created_at"`
}

// RequestItem 好友申请项。
type RequestItem struct {
	ID        int64            `json:"id,string"`
	FromUser  model.PublicUser `json:"from_user"`
	ToUser    model.PublicUser `json:"to_user"`
	Message   string           `json:"message"`
	Status    int8             `json:"status"`
	CreatedAt time.Time        `json:"created_at"`
}

// SendRequest 发起好友申请。幂等处理重复申请，避免刷接口。
func (s *FriendService) SendRequest(ctx context.Context, from int64, req SendFriendReq) error {
	if req.ToUserID <= 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "请选择要添加的用户")
	}
	if req.ToUserID == from {
		return resp.WithMsg(resp.ErrInvalidParam, "不能添加自己为好友")
	}
	if len([]rune(req.Message)) > 100 {
		return resp.WithMsg(resp.ErrInvalidParam, "验证消息最长 100 个字符")
	}

	target, err := s.deps.Users.GetByID(ctx, req.ToUserID)
	if err != nil {
		return err
	}
	if target == nil {
		return resp.WithMsg(resp.ErrNotFound, "用户不存在")
	}

	if ok, err := s.rel.AreFriends(ctx, from, req.ToUserID); err != nil {
		return err
	} else if ok {
		return resp.WithMsg(resp.ErrConflict, "你们已经是好友了")
	}

	// 被对方拉黑时不暴露真实原因，统一提示，避免被用于探测
	blocked, err := s.rel.EitherBlocked(ctx, from, req.ToUserID)
	if err != nil {
		return err
	}
	if blocked {
		return resp.WithMsg(resp.ErrForbidden, "暂时无法添加对方为好友")
	}

	pending, err := s.deps.Friends.FindPendingRequest(ctx, from, req.ToUserID)
	if err != nil {
		return err
	}
	if pending != nil {
		return resp.WithMsg(resp.ErrConflict, "已发送过申请，请等待对方处理")
	}

	row := &model.FriendRequest{
		FromUserID: from,
		ToUserID:   req.ToUserID,
		Message:    req.Message,
		Status:     model.FriendRequestPending,
	}
	if err := s.deps.Friends.CreateRequest(ctx, row); err != nil {
		return err
	}

	// 实时提醒对方；对方不在线时消息会留在申请列表里，不丢
	me, err := s.deps.Users.GetByID(ctx, from)
	if err == nil && me != nil {
		s.deps.Pusher.PushToUser(req.ToUserID, NewFrame(FrameFriendRequest, map[string]any{
			"request_id": row.ID,
			"from_user":  me.ToPublic(),
			"message":    req.Message,
		}).Encode())
	}
	return nil
}

// Requests 查询收到或发出的申请。
func (s *FriendService) Requests(ctx context.Context, uid int64, incoming bool) ([]RequestItem, error) {
	list, err := s.deps.Friends.ListRequests(ctx, uid, incoming, 50)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(list)*2)
	for _, r := range list {
		ids = append(ids, r.FromUserID, r.ToUserID)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]RequestItem, 0, len(list))
	for _, r := range list {
		out = append(out, RequestItem{
			ID:        r.ID,
			FromUser:  users[r.FromUserID],
			ToUser:    users[r.ToUserID],
			Message:   r.Message,
			Status:    r.Status,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// HandleRequest 同意或拒绝好友申请。同意时在一个事务里建立双向关系。
func (s *FriendService) HandleRequest(ctx context.Context, uid, requestID int64, accept bool) error {
	req, err := s.deps.Friends.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if req == nil {
		return resp.WithMsg(resp.ErrNotFound, "申请不存在")
	}
	if req.ToUserID != uid {
		return resp.WithMsg(resp.ErrForbidden, "无权处理该申请")
	}
	if req.Status != model.FriendRequestPending {
		return resp.WithMsg(resp.ErrConflict, "该申请已处理")
	}

	status := model.FriendRequestRejected
	if accept {
		status = model.FriendRequestAccepted
	}

	// 状态检查在事务外，两个端同时点「同意」/「拒绝」时都会读到 pending，
	// 因此事务内必须用「带 status 条件的更新」做乐观锁：只有第一个能改成功。
	err = s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.FriendRequest{}).
			Where("id = ? AND status = ?", requestID, model.FriendRequestPending).
			Update("status", status)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errRequestHandled
		}
		if !accept {
			return nil
		}
		rows := []model.Friend{
			{UserID: req.FromUserID, FriendID: req.ToUserID},
			{UserID: req.ToUserID, FriendID: req.FromUserID},
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
	})
	if errors.Is(err, errRequestHandled) {
		return resp.WithMsg(resp.ErrConflict, "该申请已处理")
	}
	if err != nil {
		return fmt.Errorf("处理好友申请失败: %w", err)
	}

	s.rel.InvalidateFriend(ctx, req.FromUserID, req.ToUserID)
	_ = s.deps.Cache.DelFriendIDs(ctx, req.FromUserID, req.ToUserID)

	if accept {
		me, _ := s.deps.Users.GetByID(ctx, uid)
		if me != nil {
			s.deps.Pusher.PushToUser(req.FromUserID, NewFrame(FrameFriendAccept, map[string]any{
				"user": me.ToPublic(),
			}).Encode())
		}
	}
	return nil
}

// List 好友列表，附带备注与星标。
func (s *FriendService) List(ctx context.Context, uid int64) ([]FriendItem, error) {
	rows, err := s.deps.Friends.ListFriends(ctx, uid)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FriendID)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]FriendItem, 0, len(rows))
	for _, r := range rows {
		u, ok := users[r.FriendID]
		if !ok {
			continue
		}
		out = append(out, FriendItem{
			PublicUser: u,
			Remark:     r.Remark,
			Starred:    r.Starred,
			CreatedAt:  r.CreatedAt,
		})
	}
	return out, nil
}

// Delete 删除好友（双向）。
func (s *FriendService) Delete(ctx context.Context, uid, friendID int64) error {
	if friendID <= 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "好友 ID 非法")
	}
	if err := s.deps.Friends.DelFriends(ctx, uid, friendID); err != nil {
		return err
	}
	s.rel.InvalidateFriend(ctx, uid, friendID)
	_ = s.deps.Cache.DelFriendIDs(ctx, uid, friendID)
	return nil
}

// Update 修改好友备注或星标。
func (s *FriendService) Update(ctx context.Context, uid, friendID int64, remark *string, starred *bool) error {
	fields := map[string]any{}
	if remark != nil {
		if len([]rune(*remark)) > 16 {
			return resp.WithMsg(resp.ErrInvalidParam, "备注最长 16 个字符")
		}
		fields["remark"] = *remark
	}
	if starred != nil {
		fields["starred"] = *starred
	}
	if len(fields) == 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "没有需要更新的字段")
	}
	if ok, err := s.rel.AreFriends(ctx, uid, friendID); err != nil {
		return err
	} else if !ok {
		return resp.WithMsg(resp.ErrForbidden, "对方不是你的好友")
	}
	return s.deps.Friends.UpdateFriend(ctx, uid, friendID, fields)
}

// Block 拉黑用户。保留好友关系但阻断消息与朋友圈互见。
func (s *FriendService) Block(ctx context.Context, uid, targetID int64) error {
	if targetID == uid {
		return resp.WithMsg(resp.ErrInvalidParam, "不能拉黑自己")
	}
	if err := s.deps.Friends.Block(ctx, uid, targetID); err != nil {
		return err
	}
	s.rel.InvalidateFriend(ctx, uid, targetID)
	_ = s.deps.Cache.DelBlockedIDs(ctx, uid)
	return nil
}

// Unblock 解除拉黑。
func (s *FriendService) Unblock(ctx context.Context, uid, targetID int64) error {
	if err := s.deps.Friends.Unblock(ctx, uid, targetID); err != nil {
		return err
	}
	s.rel.InvalidateFriend(ctx, uid, targetID)
	_ = s.deps.Cache.DelBlockedIDs(ctx, uid)
	return nil
}

// Blocked 黑名单列表。
func (s *FriendService) Blocked(ctx context.Context, uid int64) ([]model.PublicUser, error) {
	rows, err := s.deps.Friends.ListBlocked(ctx, uid)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.BlockedUserID)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]model.PublicUser, 0, len(rows))
	for _, r := range rows {
		if u, ok := users[r.BlockedUserID]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}
