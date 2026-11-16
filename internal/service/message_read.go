package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// 本文件是消息读路径：历史翻页、关键词检索、会话列表与未读汇总、已读上报、会话个性化设置。
// 类型定义与共享基元在 message.go；发送链路在 message_send.go；撤回在 message_recall.go。

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
	out, err := s.withSenders(ctx, list)
	if err != nil {
		return nil, err
	}
	// 仓储按 seq 倒序取（才能用 LIMIT 拿到「最近的一页」），历史消息要正序渲染。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Search 在指定会话内按关键词检索消息，按 seq 倒序返回（新消息在前）。
//
// 与 History 的差别不只是「多一个 WHERE」：检索结果没有翻页游标，
// 一次给最近的 limit 条即可 —— 关键词检索的意图是「找到那句话」，
// 要往下翻就换个更准的关键词，而不是继续拉第 20 页。
func (s *MessageService) Search(ctx context.Context, uid int64, convID, keyword string, limit int) ([]MessageDTO, error) {
	// 参数校验排在鉴权之前：这一步不查库、也不泄露会话是否存在，
	// 让非法请求连一次 DB 往返都省下来。权限校验仍然早于消息查询。
	kw, err := normalizeKeyword(keyword)
	if err != nil {
		return nil, err
	}
	if err := s.checkConvAccess(ctx, uid, convID); err != nil {
		return nil, err
	}
	list, err := s.deps.Messages.SearchByConv(ctx, convID, kw, normalizeSearchLimit(limit))
	if err != nil {
		return nil, err
	}
	return s.withSenders(ctx, list)
}

// normalizeSearchLimit 归一化检索条数：缺省 20，超限压到上限。
//
// 超限是「压到 50」而不是像 History 那样退回默认值 —— 退回默认值会让 limit=51
// 只拿到 20 条，比 limit=50 的 50 条还少，非单调的结果只会被当成 bug 报上来。
func normalizeSearchLimit(limit int) int {
	if limit <= 0 {
		return defaultSearchLimit
	}
	if limit > maxSearchLimit {
		return maxSearchLimit
	}
	return limit
}

// normalizeKeyword 校验并裁剪检索关键词，返回可直接进 SQL 的值。
//
// 按 rune 而不是字节计数：一个汉字占 3 字节，按字节算上限 32 就只剩 10 个汉字，
// 中文用户随手一句关键词就被判超长。裁剪首尾空白是必须的 —— 用户从聊天记录里
// 复制关键词常带上空格，带空格的 pattern 匹配不到任何东西，看起来就像「搜不到」。
func normalizeKeyword(keyword string) (string, error) {
	kw := strings.TrimSpace(keyword)
	if n := utf8.RuneCountInString(kw); n < minKeywordRunes || n > maxKeywordRunes {
		return "", resp.WithMsg(resp.ErrInvalidParam,
			fmt.Sprintf("关键词长度需在 %d~%d 字之间", minKeywordRunes, maxKeywordRunes))
	}
	return kw, nil
}

// withSenders 批量补发送者信息并把消息转成 DTO，顺序与入参一致。
//
// 历史列表与关键词检索共用这一段：逐条查用户会让一页 50 条消息变成 51 次查询，
// 而发送者去重后通常只有个位数。抽出来也是为了让「哪条读路径忘了补用户信息」
// 这种漏改只可能发生在一个地方。
func (s *MessageService) withSenders(ctx context.Context, list []model.Message) ([]MessageDTO, error) {
	out := make([]MessageDTO, 0, len(list))
	if len(list) == 0 {
		return out, nil
	}
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
	for i := range list {
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

// UnreadSummary 返回某用户的未读汇总。
//
// 数据源只有 im:unread:{uid} 一个 hash：写扩散发消息时自增、上报已读时 HDEL，
// 是唯一读时零计算的来源。群会话的未读不在这里 —— 读扩散下它等于群最新 seq
// 减我的已读 seq，要拿群表才能算，那是会话列表的职责，这里不重复一条查询链。
//
// Redis 异常时降级成空汇总而不是报错：这个接口只用来渲染红点，缓存挂掉时
// 显示「无未读」比整个首屏报错更可接受，计数本身也会随下一次自增/已读重建。
// 降级必须留 Warn，否则「红点再也不出现」会被当成前端问题排查很久。
func (s *MessageService) UnreadSummary(ctx context.Context, uid int64) *UnreadSummaryDTO {
	counts, err := s.deps.Cache.UnreadCounts(ctx, uid)
	if err != nil {
		slog.Warn("未读汇总降级为空", "uid", uid, "err", err)
		return &UnreadSummaryDTO{Items: []UnreadItem{}}
	}
	total, items := summarizeUnread(counts)
	return &UnreadSummaryDTO{Total: total, Items: items}
}

// summarizeUnread 把原始计数整理成按未读降序的列表。
//
// 排序放在服务端：客户端要的是「先看哪个会话」，这个顺序只有拿到全量计数才能定。
// 未读相同的按 conv_id 升序，保证同一份数据每次返回的顺序都一样 ——
// map 遍历本身随机，少了这一级，同样的未读会让红点顺序来回跳。
func summarizeUnread(counts map[string]int64) (int64, []UnreadItem) {
	items := make([]UnreadItem, 0, len(counts))
	var total int64
	for convID, n := range counts {
		// 已读清零走 HDEL，正常不会有 0 或负数；这里再兜一次，
		// 免得旧的脏 field 撑大列表并让 total 对不上客户端看到的红点。
		if n <= 0 {
			continue
		}
		total += n
		items = append(items, UnreadItem{ConvID: convID, Unread: n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Unread != items[j].Unread {
			return items[i].Unread > items[j].Unread
		}
		return items[i].ConvID < items[j].ConvID
	})
	return total, items
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

// MemberIDs 暴露群成员 ID 列表，供接入层做广播（如正在输入提示）。
func (s *MessageService) MemberIDs(ctx context.Context, gid int64) ([]int64, error) {
	return s.rel.MemberIDs(ctx, gid)
}
