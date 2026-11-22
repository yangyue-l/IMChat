package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"imchat/internal/model"
	"imchat/internal/pkg/media"
	"imchat/internal/pkg/resp"
)

// 本文件是消息发送链路：私聊/群聊入口 → 落库与投递 → 引用校验 → 幂等回查 → 离线补偿。
// 类型定义与跨链路共享的转换、鉴权基元在 message.go；读路径在 message_read.go；
// 撤回在 message_recall.go。

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

	// 2. 引用校验。放在幂等判定之后：同一条消息重试时若被引用消息刚被撤回，
	// 先判定重试命中就会直接返回已落库的消息，不会被新状态误判成发送失败。
	if err := s.attachQuote(ctx, sc); err != nil {
		return nil, err
	}

	// 3. 分配会话内序号，Redis 单线程执行脚本保证严格递增。
	//
	// 返回 0 说明序号键不存在，有三种来源：新会话的第一条消息、Redis 重启/淘汰
	// 弄丢了键、以及 seqTTL 到期被回收。三者若不处理，seq 会从 1 与库里历史重叠，
	// 客户端的断档判断就此失效，所以统一回源一次 MAX(seq) 再原子初始化，
	// 三种情况都走同一条路径 —— 也就是说这条回源分支是常态，不是异常兜底。
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

	// 图片消息的 content 与 extra.url 都是前端回传的绝对地址，写入前统一还原成
	// 对象 key：库里只存 key，换 CDN 时 messages 表不用回刷。
	content, extra := sc.req.Content, sc.req.Extra
	if sc.req.MsgType == model.MsgImage {
		content = media.Key(content)
		extra = stripExtraURL(extra, sc.req.Content)
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
		Content:     content,
		Extra:       extra,
	}

	// 4. 落库 + 刷新会话指针，必须同一事务。
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
		//   - uk_conv_seq：序号被抢占（seq 键过期后的回源窗口、人为删键或旧数据混入），回查不到消息。
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

	// 5. 未读计数在事务外：Redis 与 MySQL 无法同事务，只能保证「尽力而为 + 可重算」。
	// 私聊未读失真的兜底是会话行里的 last_msg_id（点进去就能看到）；
	// 群未读本身不落存储，由 last_msg_seq − read_seq 实时算出，天然可自愈。
	if sc.convType == model.ConvPrivate {
		if err := s.deps.Cache.IncrUnread(ctx, sc.convID, sc.receivers, 1); err != nil {
			slog.Warn("未读数自增失败", "conv_id", sc.convID, "err", err)
		}
	}

	// 6. 在线推送。只序列化一次，广播时所有连接复用同一份字节
	sender, err := s.deps.Users.GetByID(ctx, sc.from)
	var fromPublic *model.PublicUser
	if err == nil && sender != nil {
		p := sender.ToPublic()
		fromPublic = &p
	}
	dto := toDTO(msg, fromPublic)
	payload := NewFrame(FrameMessage, dto).Encode()
	delivered := 0
	if len(sc.receivers) > 0 {
		delivered = s.deps.Pusher.PushToUsers(sc.receivers, payload)
	}

	// 7. 离线补偿：本地一条都没投出去，且集群里也没人在线时才投 MQ。
	// delivered > 0 就说明有人在收，不必再问一次 Redis。
	if len(sc.receivers) > 0 && delivered == 0 && !s.anyRemoteOnline(ctx, sc.receivers) {
		s.publishOffline(ctx, msg)
	}
	return dto, nil
}

// attachQuote 校验被引用消息，并把引用摘要并入 req.Extra。
//
// 摘要写进 Extra 而不是单开一列：Extra 本来就是「消息的附加信息」出口，
// 加一列 varchar 要让 messages 的每次查询都多带一列，而引用只在渲染时读一次。
func (s *MessageService) attachQuote(ctx context.Context, sc *sendContext) error {
	if sc.req.QuoteMsgID == 0 {
		return nil
	}
	if sc.req.QuoteMsgID < 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "引用消息 ID 非法")
	}
	q, err := s.deps.Messages.GetByID(ctx, sc.req.QuoteMsgID)
	// 记录不存在与查询失败要分开：前者是被引用消息真的没了，属于参数问题；
	// 后者是 DB 抖动，必须原样上抛，不能让一次读失败把用户的消息打成「引用非法」。
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err := checkQuote(q, sc.convID); err != nil {
		return err
	}
	extra, err := mergeQuoteExtra(sc.req.Extra, &QuoteDTO{
		MsgID: q.ID, FromID: q.FromID, Preview: quotePreview(q),
	})
	if err != nil {
		return err
	}
	sc.req.Extra = extra
	return nil
}

// checkQuote 校验引用目标：必须存在、与被引用方同一会话、且未被撤回。
// 拆成纯函数是为了能在没有 MySQL 的环境里把三条分支全测到。
func checkQuote(q *model.Message, convID string) error {
	if q == nil || q.ID == 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "引用的消息不存在")
	}
	if q.ConvID != convID {
		return resp.WithMsg(resp.ErrInvalidParam, "只能引用同一会话内的消息")
	}
	if q.Status == model.MsgStatusRecalled {
		return resp.WithMsg(resp.ErrInvalidParam, "引用的消息已被撤回")
	}
	return nil
}

// mergeQuoteExtra 把引用摘要写进 extra 的 quote 字段，同时保留客户端已有的其它字段。
//
// 用 map[string]json.RawMessage 而不是固定结构体：extra 现在还承载图片宽高这类
// 客户端自定义字段，换成结构体等于在序列化时把它们静默丢掉。
func mergeQuoteExtra(extra string, q *QuoteDTO) (string, error) {
	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(extra) != "" {
		if err := json.Unmarshal([]byte(extra), &fields); err != nil {
			return "", resp.WithMsg(resp.ErrInvalidParam, "扩展信息必须是 JSON 对象")
		}
	}
	// extra 传 "null" 时 Unmarshal 不报错但把 map 置成 nil，直接赋值会 panic。
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return "", resp.WithMsg(resp.ErrInvalidParam, "引用信息无法序列化")
	}
	fields["quote"] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return "", resp.WithMsg(resp.ErrInvalidParam, "扩展信息无法序列化")
	}
	// 合并后要重新过一遍列宽：extra 本身接近 1024 时，多出来的引用信息会把它
	// 顶到 1406，那样前端只能收到一个不含原因的 500。
	if len(out) > maxExtraBytes {
		return "", resp.WithMsg(resp.ErrInvalidParam, "扩展信息过长，无法附加引用")
	}
	return string(out), nil
}

// quotePreview 生成引用条里的原消息摘要。图片/文件用占位文案，与会话列表摘要
// 同一套口径；正文按 rune 截断，否则中文会被切成半个字变成乱码。
func quotePreview(m *model.Message) string {
	switch m.MsgType {
	case model.MsgImage:
		return "[图片]"
	case model.MsgFile:
		return "[文件]"
	}
	r := []rune(m.Content)
	if len(r) > maxQuotePreviewRunes {
		return string(r[:maxQuotePreviewRunes]) + "..."
	}
	return m.Content
}

// anyRemoteOnline 判断接收者里是否有人在线的权威版本。
//
// 不能只看本节点：集群模式下消息由 Redis 总线送到其他节点，本地未命中不代表
// 人不在线，据本地判定就会给在线用户发离线通知。单机/集群的差异由 Presence
// 内部消化，这里不需要再关心部署模式。
// 查询失败时按「在线」处理：状态未知宁可不发离线通知，也不要重复打扰用户。
func (s *MessageService) anyRemoteOnline(ctx context.Context, uids []int64) bool {
	if s.deps.Presence == nil {
		return false
	}
	online, err := s.deps.Presence.Online(uids)
	if err != nil {
		slog.Warn("查询在线状态失败，跳过离线通知", "count", len(uids), "err", err)
		return true
	}
	return len(online) > 0
}

func (s *MessageService) publishOffline(ctx context.Context, msg *model.Message) {
	data, err := json.Marshal(map[string]any{
		"conv_id": msg.ConvID, "conv_type": msg.ConvType,
		"from_id": msg.FromID, "group_id": msg.GroupID, "seq": msg.Seq,
	})
	if err != nil {
		return
	}
	// 事件投递失败不影响发消息本身，但必须留痕：静默吞掉的话，
	// MQ 抖动期间丢了多少离线通知永远查不出来。
	if err := s.deps.Pub.Publish(ctx, Event{Type: EventOfflineNotify, Data: data}); err != nil {
		slog.Warn("离线通知事件投递失败", "conv_id", msg.ConvID, "seq", msg.Seq, "err", err)
	}
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

// isDuplicateKey 判断 MySQL 唯一键冲突（error 1062）。
func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
