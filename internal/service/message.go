package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// MessageService 消息与会话业务。
type MessageService struct {
	deps Deps
	rel  *relation
}

// 业务约束。
const (
	maxContentRunes  = 5000
	maxExtraBytes    = 1024 // 与 messages.extra 的列宽一致，超长会撞 MySQL 1406
	historyPageLimit = 50
	recallWindow     = 2 * time.Minute
)

// SendReq 发送消息参数。私聊填 ToID，群聊填 GroupID，用 ConvType 区分。
type SendReq struct {
	ConvType    int8   `json:"conv_type"`
	ToID        int64  `json:"to_id,string"`
	GroupID     int64  `json:"group_id,string"`
	MsgType     int8   `json:"msg_type"`
	Content     string `json:"content"`
	Extra       string `json:"extra"`
	ClientMsgID string `json:"client_msg_id"`
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

// SendPrivate 发送私聊消息。
func (s *MessageService) SendPrivate(ctx context.Context, from int64, req SendReq) (*MessageDTO, error) {
	if req.ToID <= 0 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "请选择接收人")
	}
	if req.ToID == from {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "不能给自己发消息")
	}
	if err := validateContent(req); err != nil {
		return nil, err
	}

	// 关系校验：好友 + 双向未拉黑。拉黑优先于好友，避免骚扰
	blocked, err := s.rel.EitherBlocked(ctx, from, req.ToID)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, resp.WithMsg(resp.ErrForbidden, "消息发送失败，对方拒绝接收")
	}
	if ok, err := s.rel.AreFriends(ctx, from, req.ToID); err != nil {
		return nil, err
	} else if !ok {
		return nil, resp.WithMsg(resp.ErrForbidden, "你们还不是好友")
	}

	convID := PrivateConvID(from, req.ToID)
	return s.persistAndDeliver(ctx, &sendContext{
		convID:    convID,
		convType:  model.ConvPrivate,
		from:      from,
		to:        req.ToID,
		groupID:   0,
		receivers: []int64{req.ToID},
		req:       req,
	})
}

// SendGroup 发送群消息。
func (s *MessageService) SendGroup(ctx context.Context, from int64, req SendReq) (*MessageDTO, error) {
	if req.GroupID <= 0 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "请选择群聊")
	}
	if err := validateContent(req); err != nil {
		return nil, err
	}

	member, err := s.rel.Member(ctx, req.GroupID, from)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, resp.WithMsg(resp.ErrForbidden, "你不在该群中")
	}
	if member.Muted && member.Role == model.GroupRoleMember {
		return nil, resp.WithMsg(resp.ErrForbidden, "你已被禁言")
	}

	members, err := s.rel.MemberIDs(ctx, req.GroupID)
	if err != nil {
		return nil, err
	}
	receivers := make([]int64, 0, len(members))
	for _, id := range members {
		if id != from {
			receivers = append(receivers, id)
		}
	}

	return s.persistAndDeliver(ctx, &sendContext{
		convID:    GroupConvID(req.GroupID),
		convType:  model.ConvGroup,
		from:      from,
		to:        0,
		groupID:   req.GroupID,
		receivers: receivers,
		req:       req,
	})
}

type sendContext struct {
	convID    string
	convType  int8
	from      int64
	to        int64
	groupID   int64
	receivers []int64
	req       SendReq
}

// persistAndDeliver 是消息写入与投递的统一路径：
// 幂等判定 -> 分配序号 -> 落库 -> 刷新会话与未读 -> 在线推送 -> 离线补偿。
func (s *MessageService) persistAndDeliver(ctx context.Context, sc *sendContext) (*MessageDTO, error) {
	// 1. 幂等：同一 client_msg_id 只处理一次，重试直接返回已落库的消息
	first, err := s.deps.Cache.MarkClientMsg(ctx, sc.from, sc.req.ClientMsgID)
	if err != nil {
		// 缓存异常不阻断发送，退化为数据库唯一索引兜底
		first = true
	}
	if !first {
		if existing := s.findByClientMsgID(ctx, sc.from, sc.req.ClientMsgID); existing != nil {
			return existing, nil
		}
	}

	// 2. 分配会话内序号，Redis 单线程执行脚本保证严格递增。
	//
	// 返回 0 说明序号键不存在：要么是新会话的第一条消息，要么 Redis 重启/淘汰
	// 把键弄丢了。后者若不处理，seq 会从 1 与库里历史重叠，客户端的断档判断
	// 就此失效，所以统一回源一次 MAX(seq) 再原子初始化，两种情况都走同一条路径。
	seq, err := s.deps.Cache.NextSeq(ctx, sc.convID)
	if err != nil {
		return nil, err
	}
	if seq == 0 {
		maxSeq, err := s.deps.Messages.MaxSeq(ctx, sc.convID)
		if err != nil {
			return nil, err
		}
		if seq, err = s.deps.Cache.InitSeq(ctx, sc.convID, maxSeq); err != nil {
			return nil, err
		}
	}

	// ID 由 model.BaseModel.BeforeCreate 统一分配，这里不再手动生成。
	msg := &model.Message{
		ConvID:      sc.convID,
		ConvType:    sc.convType,
		Seq:         seq,
		FromID:      sc.from,
		ToID:        sc.to,
		GroupID:     sc.groupID,
		ClientMsgID: sc.req.ClientMsgID,
		MsgType:     sc.req.MsgType,
		Content:     sc.req.Content,
		Extra:       sc.req.Extra,
	}

	// 3. 落库 + 刷新会话指针，必须同一事务。
	//
	// 拆开写的后果是真丢状态：
	//   - 私聊：消息进库了但会话行没刷新，列表看不到这条消息，而未读数照样 +1，
	//     用户看到「有红点却点不进去」，只有重新发一条才能自愈；
	//   - 群聊：群表 last_msg_seq 没更新，读扩散下未读数按 seq 差算，
	//     全群成员的未读都会少算，直到下一条消息把 seq 追上去。
	//
	// 唯一索引冲突说明并发重复投递，回查返回同一结果。
	preview := previewOf(msg)
	now := time.Now()
	err = s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.deps.Messages.WithTx(tx).Insert(ctx, msg); err != nil {
			return err
		}
		if sc.convType == model.ConvPrivate {
			rows := []model.Conversation{
				{
					UserID: sc.from, ConvID: sc.convID, ConvType: model.ConvPrivate, TargetID: sc.to,
					LastMsgID: msg.ID, LastMsgPreview: preview, LastMsgType: msg.MsgType, LastMsgAt: now,
				},
				{
					UserID: sc.to, ConvID: sc.convID, ConvType: model.ConvPrivate, TargetID: sc.from,
					LastMsgID: msg.ID, LastMsgPreview: preview, LastMsgType: msg.MsgType, LastMsgAt: now,
				},
			}
			return s.deps.Messages.WithTx(tx).UpsertConversations(ctx, rows)
		}
		return s.deps.Groups.WithTx(tx).UpdateLastMessage(ctx, sc.groupID, msg, preview, now)
	})
	if err != nil {
		// 1062 有两个来源，必须分开处理：
		//   - uk_from_client：并发重复投递，回查到已落库的消息直接返回，语义上等同于成功；
		//   - uk_conv_seq：序号被抢占（Redis 丢键、人为删键或旧数据混入），回查不到消息。
		//     此时计数器已经越过冲突值，客户端拿同一个 client_msg_id 重试就会分配到
		//     新序号并成功，所以回一条可重试的冲突，而不是让它变成看不懂的 500。
		if isDuplicateKey(err) {
			if existing := s.findByClientMsgID(ctx, sc.from, sc.req.ClientMsgID); existing != nil {
				return existing, nil
			}
			return nil, resp.WithMsg(resp.ErrConflict, "消息序号冲突，请重试")
		}
		return nil, err
	}

	// 4. 未读计数在事务外：Redis 与 MySQL 无法同事务，只能保证「尽力而为 + 可重算」。
	// 私聊未读失真的兜底是会话行里的 last_msg_id（点进去就能看到）；
	// 群未读本身不落存储，由 last_msg_seq − read_seq 实时算出，天然可自愈。
	if sc.convType == model.ConvPrivate {
		if err := s.deps.Cache.IncrUnread(ctx, sc.convID, sc.receivers, 1); err != nil {
			slog.Warn("未读数自增失败", "conv_id", sc.convID, "err", err)
		}
	}

	// 5. 在线推送。只序列化一次，广播时所有连接复用同一份字节
	sender, err := s.deps.Users.GetByID(ctx, sc.from)
	var fromPublic *model.PublicUser
	if err == nil && sender != nil {
		p := sender.ToPublic()
		fromPublic = &p
	}
	dto := toDTO(msg, fromPublic)
	payload := NewFrame(FrameMessage, dto).Encode()
	if len(sc.receivers) > 0 {
		s.deps.Pusher.PushToUsers(sc.receivers, payload)
	}

	// 6. 离线补偿：全部接收者都不在线时投递到 MQ，由消费者做离线通知
	if len(sc.receivers) > 0 && !s.anyLocalOnline(sc.receivers) {
		s.publishOffline(ctx, msg)
	}
	return dto, nil
}

func (s *MessageService) anyLocalOnline(uids []int64) bool {
	for _, uid := range uids {
		if s.deps.Pusher.LocalOnline(uid) {
			return true
		}
	}
	return false
}

func (s *MessageService) publishOffline(ctx context.Context, msg *model.Message) {
	data, err := json.Marshal(map[string]any{
		"conv_id": msg.ConvID, "conv_type": msg.ConvType,
		"from_id": msg.FromID, "group_id": msg.GroupID, "seq": msg.Seq,
	})
	if err != nil {
		return
	}
	_ = s.deps.Pub.Publish(ctx, Event{Type: EventOfflineNotify, Data: data})
}

// findByClientMsgID 幂等回查。
func (s *MessageService) findByClientMsgID(ctx context.Context, from int64, clientMsgID string) *MessageDTO {
	var msg model.Message
	err := s.deps.DB.WithContext(ctx).
		Where("from_id = ? AND client_msg_id = ?", from, clientMsgID).Limit(1).Find(&msg).Error
	if err != nil || msg.ID == 0 {
		return nil
	}
	return toDTO(&msg, nil)
}

// History 拉取历史消息。游标分页，按 seq 倒序取 limit 条后翻转为正序返回。
func (s *MessageService) History(ctx context.Context, uid int64, convID string, beforeSeq int64, limit int) ([]MessageDTO, error) {
	if limit <= 0 || limit > historyPageLimit {
		limit = 20
	}
	if err := s.checkConvAccess(ctx, uid, convID); err != nil {
		return nil, err
	}

	list, err := s.deps.Messages.ListByConv(ctx, convID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MessageDTO, 0, len(list))
	// 批量补发送者信息，避免每条消息一次查询
	uidSet := make(map[int64]struct{}, len(list))
	for i := range list {
		uidSet[list[i].FromID] = struct{}{}
	}
	ids := make([]int64, 0, len(uidSet))
	for id := range uidSet {
		ids = append(ids, id)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := len(list) - 1; i >= 0; i-- {
		u := users[list[i].FromID]
		out = append(out, *toDTO(&list[i], &u))
	}
	return out, nil
}

// Conversations 会话列表。
//
// 私聊走写扩散：conversations 表双方各一行，未读来自 Redis 计数，读时零计算。
// 群聊走读扩散：不预建会话行，靠成员关系 + 群表冗余的最新消息字段组装，
// 未读用「群最新 seq − 我的已读 seq」实时算出，因此发布消息不再需要写 N 次。
func (s *MessageService) Conversations(ctx context.Context, uid int64) ([]ConversationDTO, error) {
	rows, err := s.deps.Messages.ListConversations(ctx, uid, 200)
	if err != nil {
		return nil, err
	}

	// 群会话直接按成员关系拉取，天然过滤掉已退群 / 被移除的群
	members, err := s.deps.Groups.ListAllMemberGroups(ctx, uid)
	if err != nil {
		return nil, err
	}
	gids := make([]int64, 0, len(members))
	for _, m := range members {
		gids = append(gids, m.GroupID)
	}
	groups, err := s.deps.Groups.GetMany(ctx, gids)
	if err != nil {
		return nil, err
	}

	// conversations 里的群行只承载个性化设置（置顶/免打扰），不承载消息摘要
	privateRows := make([]model.Conversation, 0, len(rows))
	settings := make(map[string]model.Conversation, len(rows))
	for _, r := range rows {
		if r.ConvType == model.ConvPrivate {
			privateRows = append(privateRows, r)
			continue
		}
		settings[r.ConvID] = r
	}

	// 私聊未读来自计数器；群未读需要已读序号
	unread, err := s.deps.Cache.UnreadCounts(ctx, uid)
	if err != nil {
		unread = map[string]int64{}
	}
	readSeqs, err := s.deps.Cache.ReadSeqs(ctx, uid)
	if err != nil {
		readSeqs = map[string]int64{}
	}

	userIDs := make([]int64, 0, len(privateRows))
	for _, r := range privateRows {
		userIDs = append(userIDs, r.TargetID)
	}
	users, err := s.rel.UserMap(ctx, userIDs)
	if err != nil {
		return nil, err
	}

	out := make([]ConversationDTO, 0, len(privateRows)+len(groups))
	for _, r := range privateRows {
		item := ConversationDTO{
			ConvID: r.ConvID, ConvType: model.ConvPrivate, TargetID: r.TargetID,
			LastMsgID: r.LastMsgID, LastMsgPreview: r.LastMsgPreview,
			LastMsgType: r.LastMsgType, LastMsgAt: r.LastMsgAt,
			Unread: unread[r.ConvID], Pinned: r.Pinned, Muted: r.Muted,
		}
		if u, ok := users[r.TargetID]; ok {
			item.Name, item.Avatar = u.Nickname, u.Avatar
		}
		out = append(out, item)
	}

	// Redis 未命中时回源 DB，避免 Redis 重启后把已读记录当成"从未读过"
	missing := make([]string, 0, len(groups))
	for i := range groups {
		convID := GroupConvID(groups[i].ID)
		if _, ok := readSeqs[convID]; !ok {
			missing = append(missing, convID)
		}
	}
	if len(missing) > 0 {
		if fromDB, err := s.deps.Messages.ListReadSeqs(ctx, uid, missing); err == nil {
			for k, v := range fromDB {
				readSeqs[k] = v
			}
		}
	}

	// 仍然缺失的会话说明从未上报过已读，按"从当前位置开始"处理，
	// 否则入群前的历史消息会一次性变成几千条未读。
	pendingInit := make(map[string]int64, 8)

	for i := range groups {
		g := &groups[i]
		if g.Status != model.GroupStatusNormal {
			continue
		}
		convID := GroupConvID(g.ID)
		readSeq, hasRead := readSeqs[convID]
		if !hasRead {
			readSeq = g.LastMsgSeq
			if g.LastMsgSeq > 0 && len(pendingInit) < 100 {
				pendingInit[convID] = g.LastMsgSeq
			}
		}
		item := ConversationDTO{
			ConvID: convID, ConvType: model.ConvGroup, TargetID: g.ID,
			Name: g.Name, Avatar: g.Avatar, MemberCount: g.MemberCount,
			LastMsgID: g.LastMsgID, LastMsgPreview: g.LastMsgPreview,
			LastMsgType: g.LastMsgType,
			Unread:      unreadOfGroup(g.LastMsgSeq, readSeq),
		}
		if g.LastMsgAt != nil {
			item.LastMsgAt = *g.LastMsgAt
		} else {
			item.LastMsgAt = g.CreatedAt
		}
		if st, ok := settings[convID]; ok {
			item.Pinned, item.Muted = st.Pinned, st.Muted
		}
		out = append(out, item)
	}

	if len(pendingInit) > 0 {
		s.alignReadSeqs(ctx, uid, pendingInit)
	}

	// 置顶优先，其次按最后消息时间倒序；从未产生消息的群自然沉底
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].LastMsgAt.After(out[j].LastMsgAt)
	})
	return out, nil
}

// alignReadSeqs 把从未上报已读的群会话对齐到当前位置，避免历史消息被算成未读。
// 数量上限 100，超出的留到下次，保证单次请求不被写放大拖慢。
func (s *MessageService) alignReadSeqs(ctx context.Context, uid int64, seqs map[string]int64) {
	for convID, seq := range seqs {
		_ = s.deps.Cache.SetReadSeqIfAbsent(ctx, uid, convID, seq)
		_ = s.deps.Messages.UpsertRead(ctx, uid, convID, seq)
	}
}

// unreadOfGroup 读扩散下的群未读 = 群最新序号 - 我的已读序号。
func unreadOfGroup(lastSeq, readSeq int64) int64 {
	if lastSeq <= readSeq {
		return 0
	}
	return lastSeq - readSeq
}

// MarkRead 上报已读位置：DB 持久化 + Redis 计数清零 + 回执推送。
func (s *MessageService) MarkRead(ctx context.Context, uid int64, convID string, seq int64) error {
	if seq < 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "已读位置非法")
	}
	if err := s.checkConvAccess(ctx, uid, convID); err != nil {
		return err
	}
	// 先定位问题：DB 用 GREATEST 只前进，Redis 侧也必须同样只前进，
	// 所以这里走原子脚本，拿到的 effective 才是两边一致的「已读位置」，
	// 回执也用它。裸 HSET 会让乱序上报把已读推回去，群未读立刻反弹。
	effective := seq
	if v, err := s.deps.Cache.MarkRead(ctx, uid, convID, seq); err == nil {
		effective = v
	}
	if err := s.deps.Messages.UpsertRead(ctx, uid, convID, effective); err != nil {
		return err
	}

	// 私聊回执通知对方，便于展示"已读"
	if convType, target, ok := parseConv(convID, uid); ok && convType == model.ConvPrivate {
		s.deps.Pusher.PushToUser(target, NewFrame(FrameRead, map[string]any{
			"conv_id": convID, "reader": uid, "seq": effective,
		}).Encode())
	}
	return nil
}

// Recall 撤回消息，仅发送者本人且 2 分钟内有效。
func (s *MessageService) Recall(ctx context.Context, uid, msgID int64) error {
	msg, err := s.deps.Messages.GetByID(ctx, msgID)
	if err != nil {
		return resp.WithMsg(resp.ErrNotFound, "消息不存在")
	}
	if msg.FromID != uid {
		return resp.WithMsg(resp.ErrForbidden, "只能撤回自己发送的消息")
	}
	if time.Since(msg.CreatedAt) > recallWindow {
		return resp.WithMsg(resp.ErrForbidden, "超过 2 分钟的消息无法撤回")
	}
	if msg.Status == model.MsgStatusRecalled {
		return nil
	}
	if err := s.deps.Messages.Recall(ctx, msgID); err != nil {
		return err
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

// MemberIDs 暴露群成员 ID 列表，供接入层做广播（如正在输入提示）。
func (s *MessageService) MemberIDs(ctx context.Context, gid int64) ([]int64, error) {
	return s.rel.MemberIDs(ctx, gid)
}

// SetConversationFlag 设置置顶/免打扰。
// 读扩散后群会话不再预建行，用户首次设置时才落库，所以这里用 UPSERT。
func (s *MessageService) SetConversationFlag(ctx context.Context, uid int64, convID string, pinned, muted *bool) error {
	if pinned == nil && muted == nil {
		return resp.WithMsg(resp.ErrInvalidParam, "没有需要更新的字段")
	}
	convType, target, ok := parseConv(convID, uid)
	if !ok {
		return resp.WithMsg(resp.ErrForbidden, "无权访问该会话")
	}
	row := model.Conversation{UserID: uid, ConvID: convID, ConvType: convType, TargetID: target}
	if pinned != nil {
		row.Pinned = *pinned
	}
	if muted != nil {
		row.Muted = *muted
	}
	return s.deps.Messages.UpsertConversationSettings(ctx, row)
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

func validateContent(req SendReq) error {
	if req.ClientMsgID == "" {
		return resp.WithMsg(resp.ErrInvalidParam, "缺少 client_msg_id，无法保证消息幂等")
	}
	if len(req.ClientMsgID) > 64 {
		return resp.WithMsg(resp.ErrInvalidParam, "client_msg_id 过长")
	}
	switch req.MsgType {
	case model.MsgText, model.MsgImage, model.MsgFile:
	default:
		return resp.WithMsg(resp.ErrInvalidParam, "不支持的消息类型")
	}
	if utf8.RuneCountInString(req.Content) > maxContentRunes {
		return resp.WithMsg(resp.ErrInvalidParam, "消息内容过长")
	}
	// extra 落的是 varchar(1024)，这里按字节数卡：严格模式下超长是 1406 错误，
	// 抛到 handler 就成了 500，前端拿不到「你传的东西太大」这种可行动的信息。
	if len(req.Extra) > maxExtraBytes {
		return resp.WithMsg(resp.ErrInvalidParam, "扩展信息过长")
	}
	if strings.TrimSpace(req.Content) == "" && req.MsgType == model.MsgText {
		return resp.WithMsg(resp.ErrInvalidParam, "消息内容不能为空")
	}
	return nil
}

// previewOf 生成会话列表的摘要，图片/文件用占位文案。
func previewOf(m *model.Message) string {
	switch m.MsgType {
	case model.MsgImage:
		return "[图片]"
	case model.MsgFile:
		return "[文件]"
	default:
		r := []rune(m.Content)
		if len(r) > 40 {
			return string(r[:40]) + "..."
		}
		return m.Content
	}
}

func toDTO(m *model.Message, from *model.PublicUser) *MessageDTO {
	return &MessageDTO{
		ID: m.ID, ConvID: m.ConvID, ConvType: m.ConvType, Seq: m.Seq,
		FromID: m.FromID, ToID: m.ToID, GroupID: m.GroupID,
		ClientMsgID: m.ClientMsgID, MsgType: m.MsgType, Content: m.Content,
		Extra: m.Extra, Status: m.Status, CreatedAt: m.CreatedAt, FromUser: from,
	}
}

// isDuplicateKey 判断 MySQL 唯一键冲突（error 1062）。
func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
