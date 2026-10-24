package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
	"imchat/internal/pkg/snowflake"
)

// MomentService 朋友圈业务。
type MomentService struct {
	deps Deps
	rel  *relation
}

const (
	maxMomentImages    = 9
	maxMomentRunes     = 2000
	maxCommentsPerItem = 10
	maxFeedFanout      = 500 // 单个动态最多写扩散的好友数，超过则退化为拉模式
)

// PublishMomentReq 发布动态参数。
type PublishMomentReq struct {
	Content    string   `json:"content"`
	Images     []string `json:"images"`
	Visibility int8     `json:"visibility"`
	Location   string   `json:"location"`
}

// CommentDTO 评论项。
type CommentDTO struct {
	ID        int64             `json:"id,string"`
	User      *model.PublicUser `json:"user,omitempty"`
	Content   string            `json:"content"`
	ReplyTo   *model.PublicUser `json:"reply_to,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// MomentDTO 动态项。
type MomentDTO struct {
	ID           int64              `json:"id,string"`
	User         *model.PublicUser  `json:"user,omitempty"`
	Content      string             `json:"content"`
	Images       []string           `json:"images"`
	Visibility   int8               `json:"visibility"`
	Location     string             `json:"location"`
	LikeCount    int                `json:"like_count"`
	CommentCount int                `json:"comment_count"`
	Liked        bool               `json:"liked"`
	Likes        []model.PublicUser `json:"likes"`
	Comments     []CommentDTO       `json:"comments"`
	CreatedAt    time.Time          `json:"created_at"`
	IsMine       bool               `json:"is_mine"`
}

// Publish 发布动态。写扩散交给 MQ 异步完成，接口响应不被好友数量拖慢。
func (s *MomentService) Publish(ctx context.Context, uid int64, req PublishMomentReq) (*MomentDTO, error) {
	if len([]rune(req.Content)) > maxMomentRunes {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "内容最长 2000 字")
	}
	if len(req.Images) > maxMomentImages {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "最多上传 9 张图片")
	}
	if strings.TrimSpace(req.Content) == "" && len(req.Images) == 0 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "内容不能为空")
	}
	visibility := req.Visibility
	if visibility != model.MomentPublic && visibility != model.MomentFriends && visibility != model.MomentPrivate {
		visibility = model.MomentFriends
	}

	m := &model.Moment{
		UserID: uid, Content: req.Content, Images: req.Images,
		Visibility: visibility, Location: req.Location,
	}
	if err := s.deps.Moments.Create(ctx, m); err != nil {
		return nil, err
	}

	// 异步写扩散：把动态 ID 推到好友时间线，读取时可跳过全量拉取
	if visibility != model.MomentPrivate {
		data, err := json.Marshal(map[string]any{"moment_id": m.ID, "user_id": uid})
		if err == nil {
			if err := s.deps.Pub.Publish(ctx, Event{Type: EventMomentFanout, Data: data}); err != nil {
				slog.Warn("朋友圈扇出事件投递失败", "moment_id", m.ID, "err", err)
			}
		}
	}

	me, err := s.deps.Users.GetByID(ctx, uid)
	var pub *model.PublicUser
	if err == nil && me != nil {
		p := me.ToPublic()
		pub = &p
	}
	return &MomentDTO{
		ID: m.ID, User: pub, Content: m.Content, Images: m.Images,
		Visibility: m.Visibility, Location: m.Location, CreatedAt: m.CreatedAt, IsMine: true,
	}, nil
}

// FanoutMoment 把动态写入好友的时间线缓存，由 MQ 消费者调用。
func (s *MomentService) FanoutMoment(ctx context.Context, momentID, authorID int64) error {
	friendIDs, err := s.deps.Friends.FriendIDs(ctx, authorID)
	if err != nil {
		return err
	}
	// 拉黑关系是双向的：拉黑我的人也不该看到我的动态
	blocked, err := s.deps.Friends.BlockedRelatedIDs(ctx, authorID)
	if err != nil {
		return err
	}
	blockedSet := make(map[int64]struct{}, len(blocked))
	for _, id := range blocked {
		blockedSet[id] = struct{}{}
	}
	// 过滤后再判断扇出规模，避免白写一批很快被前端剔除的索引
	friendIDs = filterOut(friendIDs, blockedSet)
	if len(friendIDs) == 0 {
		return nil
	}
	if len(friendIDs) > maxFeedFanout {
		// 好友过多时改为拉模式，避免一次写入上万次
		return nil
	}

	ts := time.Now().UnixMilli()
	for _, uid := range friendIDs {
		if err := s.deps.Cache.PushFeed(ctx, uid, momentID, ts, maxFeedFanout*2); err != nil {
			slog.Warn("写入时间线失败", "uid", uid, "moment_id", momentID, "err", err)
		}
	}
	return nil
}

// filterOut 从 ids 中移除 exclude 集合内的元素。
func filterOut(ids []int64, exclude map[int64]struct{}) []int64 {
	if len(exclude) == 0 {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if _, bad := exclude[id]; !bad {
			out = append(out, id)
		}
	}
	return out
}

// Timeline 好友动态流。优先读 Redis 时间线，命中不足则回源 MySQL 并重建。
func (s *MomentService) Timeline(ctx context.Context, uid int64, beforeID int64, limit int) ([]MomentDTO, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}

	// 快路径：Redis 时间线已缓存足够条目
	if ids, err := s.deps.Cache.FeedIDs(ctx, uid, beforeID, int64(limit)); err == nil && len(ids) >= limit {
		if list, err := s.deps.Moments.ListByIDs(ctx, ids); err == nil && len(list) > 0 {
			sort.Slice(list, func(i, j int) bool { return list[i].ID > list[j].ID })
			if dtos, err := s.assemble(ctx, uid, list); err == nil {
				return dtos, nil
			}
		}
	}

	// 慢路径：拉模式。作者集合 = 自己 + 好友 - 黑名单
	authorIDs, err := s.deps.Friends.FriendIDs(ctx, uid)
	if err != nil {
		return nil, err
	}
	authorIDs = append(authorIDs, uid)
	// 双向过滤：我拉黑的人、拉黑我的人，都不应出现在我的时间线里
	if blocked, err := s.deps.Friends.BlockedRelatedIDs(ctx, uid); err == nil && len(blocked) > 0 {
		blockedSet := make(map[int64]struct{}, len(blocked))
		for _, id := range blocked {
			blockedSet[id] = struct{}{}
		}
		filtered := authorIDs[:0]
		for _, id := range authorIDs {
			if _, ok := blockedSet[id]; !ok {
				filtered = append(filtered, id)
			}
		}
		authorIDs = filtered
	}

	list, err := s.deps.Moments.ListByUsers(ctx, authorIDs, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return s.assemble(ctx, uid, list)
}

// UserMoments 查看某人的动态（他人主页），按可见性过滤。
func (s *MomentService) UserMoments(ctx context.Context, viewerID, authorID int64, beforeID int64, limit int) ([]MomentDTO, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	if viewerID != authorID {
		blocked, err := s.rel.EitherBlocked(ctx, viewerID, authorID)
		if err != nil {
			return nil, err
		}
		if blocked {
			return nil, resp.WithMsg(resp.ErrForbidden, "无权查看该用户的动态")
		}
		isFriend, err := s.rel.AreFriends(ctx, viewerID, authorID)
		if err != nil {
			return nil, err
		}
		if !isFriend {
			return nil, resp.WithMsg(resp.ErrForbidden, "你们还不是好友")
		}
	}

	list, err := s.deps.Moments.ListByAuthor(ctx, authorID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	// 可见性过滤：私密仅作者可见，好友可见要求是好友
	filtered := make([]model.Moment, 0, len(list))
	for _, m := range list {
		if m.Visibility == model.MomentPrivate && viewerID != authorID {
			continue
		}
		filtered = append(filtered, m)
	}
	return s.assemble(ctx, viewerID, filtered)
}

// MomentDetail 查询单条动态详情，含最新评论。
func (s *MomentService) MomentDetail(ctx context.Context, uid, momentID int64) (*MomentDTO, error) {
	m, err := s.deps.Moments.Get(ctx, momentID)
	if err != nil || m == nil {
		return nil, resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	if err := s.checkMomentVisible(ctx, uid, m); err != nil {
		return nil, err
	}
	list, err := s.assemble(ctx, uid, []model.Moment{*m})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	return &list[0], nil
}

// Like 点赞，幂等。
//
// 点赞行与计数必须同事务：拆开写在「点赞成功、计数失败」时会返回 500，
// 客户端重试又被唯一索引判为重复（added=false），计数就永远少 1 且无法自愈。
func (s *MomentService) Like(ctx context.Context, uid, momentID int64) error {
	m, err := s.deps.Moments.Get(ctx, momentID)
	if err != nil || m == nil {
		return resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	if err := s.checkMomentVisible(ctx, uid, m); err != nil {
		return err
	}
	added := false
	err = s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Moments.WithTx(tx)
		ok, err := repo.AddLike(ctx, momentID, uid)
		if err != nil || !ok {
			return err
		}
		added = true
		return repo.IncrCounters(ctx, momentID, 1, 0)
	})
	if err != nil {
		return err
	}
	if added && m.UserID != uid {
		s.deps.Pusher.PushToUser(m.UserID, NewFrame(FrameNotice, map[string]any{
			"kind": "moment_like", "moment_id": momentID, "from": uid,
		}).Encode())
	}
	return nil
}

// Unlike 取消点赞。
func (s *MomentService) Unlike(ctx context.Context, uid, momentID int64) error {
	return s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Moments.WithTx(tx)
		removed, err := repo.DelLike(ctx, momentID, uid)
		if err != nil || !removed {
			return err
		}
		return repo.IncrCounters(ctx, momentID, -1, 0)
	})
}

// Comment 发表评论。
//
// 评论表没有幂等键，所以「插入成功但计数失败却返回错误」会造成用户重试产生重复评论。
// 两者同事务后，要么都成功，要么都回滚，重试是安全的。
func (s *MomentService) Comment(ctx context.Context, uid, momentID int64, content string, replyTo, parentID int64) (*CommentDTO, error) {
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 500 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "评论内容需为 1-500 字")
	}
	m, err := s.deps.Moments.Get(ctx, momentID)
	if err != nil || m == nil {
		return nil, resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	if err := s.checkMomentVisible(ctx, uid, m); err != nil {
		return nil, err
	}

	c := &model.MomentComment{
		MomentID: momentID, UserID: uid, Content: content,
		ReplyToUserID: replyTo, ParentID: parentID,
	}
	err = s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Moments.WithTx(tx)
		if err := repo.AddComment(ctx, c); err != nil {
			return err
		}
		return repo.IncrCounters(ctx, momentID, 0, 1)
	})
	if err != nil {
		return nil, err
	}

	users, err := s.rel.UserMap(ctx, []int64{uid, replyTo})
	if err != nil {
		return nil, err
	}
	me := users[uid]
	dto := &CommentDTO{ID: c.ID, User: &me, Content: c.Content, CreatedAt: c.CreatedAt}
	if u, ok := users[replyTo]; ok && replyTo != 0 {
		dto.ReplyTo = &u
	}

	// 通知动态作者（自己评论自己不通知）
	if m.UserID != uid {
		s.deps.Pusher.PushToUser(m.UserID, NewFrame(FrameNotice, map[string]any{
			"kind": "moment_comment", "moment_id": momentID, "from": uid, "content": content,
		}).Encode())
	}
	return dto, nil
}

// DeleteComment 删除评论，仅评论者或动态作者可删。
//
// 删一级评论时连带删掉它下面的回复：只删父节点会让 parent_id 悬空，
// 前端渲染不出上下文；计数也必须按实际删除条数扣，否则 comment_count 会虚高。
func (s *MomentService) DeleteComment(ctx context.Context, uid, commentID int64) error {
	c, err := s.deps.Moments.GetComment(ctx, commentID)
	if err != nil || c == nil {
		return resp.WithMsg(resp.ErrNotFound, "评论不存在")
	}
	m, err := s.deps.Moments.Get(ctx, c.MomentID)
	if err != nil || m == nil {
		return resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	if c.UserID != uid && m.UserID != uid {
		return resp.WithMsg(resp.ErrForbidden, "无权删除该评论")
	}
	return s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Moments.WithTx(tx)
		removed, err := repo.DelComment(ctx, commentID, c.UserID)
		if err != nil || !removed {
			return err
		}
		replies, err := repo.DelCommentReplies(ctx, commentID)
		if err != nil {
			return err
		}
		return repo.IncrCounters(ctx, c.MomentID, 0, -int(1+replies))
	})
}

// Delete 删除动态，仅作者本人。
func (s *MomentService) Delete(ctx context.Context, uid, momentID int64) error {
	m, err := s.deps.Moments.Get(ctx, momentID)
	if err != nil || m == nil {
		return resp.WithMsg(resp.ErrNotFound, "动态不存在")
	}
	if m.UserID != uid {
		return resp.WithMsg(resp.ErrForbidden, "只能删除自己的动态")
	}
	return s.deps.Moments.Delete(ctx, momentID, uid)
}

// checkMomentVisible 校验动态对当前用户是否可见。
func (s *MomentService) checkMomentVisible(ctx context.Context, uid int64, m *model.Moment) error {
	if m.UserID == uid {
		return nil
	}
	blocked, err := s.rel.EitherBlocked(ctx, uid, m.UserID)
	if err != nil {
		return err
	}
	if blocked {
		return resp.WithMsg(resp.ErrForbidden, "无权访问该动态")
	}
	if m.Visibility == model.MomentPrivate {
		return resp.WithMsg(resp.ErrForbidden, "无权访问该动态")
	}
	if m.Visibility == model.MomentFriends {
		ok, err := s.rel.AreFriends(ctx, uid, m.UserID)
		if err != nil {
			return err
		}
		if !ok {
			return resp.WithMsg(resp.ErrForbidden, "无权访问该动态")
		}
	}
	return nil
}

// assemble 批量补全作者、点赞、评论信息，全程避免 N+1 查询。
func (s *MomentService) assemble(ctx context.Context, viewerID int64, list []model.Moment) ([]MomentDTO, error) {
	if len(list) == 0 {
		return []MomentDTO{}, nil
	}
	uidSet := map[int64]struct{}{viewerID: {}}
	momentIDs := make([]int64, 0, len(list))
	for i := range list {
		uidSet[list[i].UserID] = struct{}{}
		momentIDs = append(momentIDs, list[i].ID)
	}

	likes, err := s.deps.Moments.LikeUserIDs(ctx, momentIDs)
	if err != nil {
		return nil, err
	}
	comments, err := s.deps.Moments.CommentsByMoments(ctx, momentIDs, maxCommentsPerItem)
	if err != nil {
		return nil, err
	}

	for _, ids := range likes {
		for _, id := range ids {
			uidSet[id] = struct{}{}
		}
	}
	for _, cs := range comments {
		for _, c := range cs {
			uidSet[c.UserID] = struct{}{}
			if c.ReplyToUserID != 0 {
				uidSet[c.ReplyToUserID] = struct{}{}
			}
		}
	}

	ids := make([]int64, 0, len(uidSet))
	for id := range uidSet {
		ids = append(ids, id)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]MomentDTO, 0, len(list))
	for i := range list {
		m := &list[i]
		dto := MomentDTO{
			ID: m.ID, Content: m.Content, Images: m.Images, Visibility: m.Visibility,
			Location: m.Location, LikeCount: m.LikeCount, CommentCount: m.CommentCount,
			CreatedAt: m.CreatedAt, IsMine: m.UserID == viewerID,
			Likes: []model.PublicUser{}, Comments: []CommentDTO{},
		}
		if u, ok := users[m.UserID]; ok {
			uu := u
			dto.User = &uu
		}
		for _, id := range likes[m.ID] {
			if u, ok := users[id]; ok {
				dto.Likes = append(dto.Likes, u)
				if id == viewerID {
					dto.Liked = true
				}
			}
		}
		for _, c := range comments[m.ID] {
			item := CommentDTO{ID: c.ID, Content: c.Content, CreatedAt: c.CreatedAt}
			if u, ok := users[c.UserID]; ok {
				uu := u
				item.User = &uu
			}
			if c.ReplyToUserID != 0 {
				if u, ok := users[c.ReplyToUserID]; ok {
					uu := u
					item.ReplyTo = &uu
				}
			}
			dto.Comments = append(dto.Comments, item)
		}
		out = append(out, dto)
	}
	return out, nil
}

// PublishSystemNotice 生成一条系统消息并推送，供群事件等场景复用。
func (s *MessageService) PublishSystemNotice(ctx context.Context, uids []int64, text string) {
	if len(uids) == 0 {
		return
	}
	s.deps.Pusher.PushToUsers(uids, NewFrame(FrameNotice, map[string]any{
		"kind": "system", "text": text, "id": snowflake.Next(),
	}).Encode())
}
