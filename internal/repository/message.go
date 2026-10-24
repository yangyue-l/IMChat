package repository

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
)

// MessageRepo 消息、会话、已读位置数据访问。
type MessageRepo struct{ db *gorm.DB }

// NewMessageRepo 创建仓库。
func NewMessageRepo(db *gorm.DB) *MessageRepo { return &MessageRepo{db: db} }

// WithTx 返回绑定到事务的副本，供 service 组装跨表原子写。
// 仓库本身无状态，复制一个只换 db 句柄的实例即可，不会污染原对象。
func (r *MessageRepo) WithTx(tx *gorm.DB) *MessageRepo { return &MessageRepo{db: tx} }

// Insert 写入消息。唯一索引 uk_from_client 保证同一发送者的 client_msg_id 只落一次。
func (r *MessageRepo) Insert(ctx context.Context, m *model.Message) error {
	return r.db.WithContext(ctx).Create(m).Error
}

// GetByID 按 ID 查询消息。
func (r *MessageRepo) GetByID(ctx context.Context, id int64) (*model.Message, error) {
	var m model.Message
	err := r.db.WithContext(ctx).First(&m, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListByConv 游标分页拉历史：以 seq 倒序取 limit 条，避免 OFFSET 深翻页。
// 走 idx_conv_seq 复合索引，代价与页深无关。
func (r *MessageRepo) ListByConv(ctx context.Context, convID string, beforeSeq int64, limit int) ([]model.Message, error) {
	var list []model.Message
	q := r.db.WithContext(ctx).Where("conv_id = ?", convID)
	if beforeSeq > 0 {
		q = q.Where("seq < ?", beforeSeq)
	}
	err := q.Order("seq DESC").Limit(limit).Find(&list).Error
	return list, err
}

// MaxSeq 返回会话当前最大序号。既是未读兜底计算的数据源，
// 也是 Redis 序号键丢失后重新初始化 seq 的基准（见 cache.NextSeq / InitSeq）。
func (r *MessageRepo) MaxSeq(ctx context.Context, convID string) (int64, error) {
	var seq int64
	err := r.db.WithContext(ctx).Model(&model.Message{}).
		Where("conv_id = ?", convID).
		Select("COALESCE(MAX(seq), 0)").Scan(&seq).Error
	return seq, err
}

// UpsertConversations 批量刷新会话列表的最后一条消息，冲突则更新。
func (r *MessageRepo) UpsertConversations(ctx context.Context, rows []model.Conversation) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "conv_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"last_msg_id", "last_msg_preview", "last_msg_type", "last_msg_at", "updated_at",
		}),
	}).Create(&rows).Error
}

// ListConversations 分页查询会话列表，置顶优先、时间倒序。
func (r *MessageRepo) ListConversations(ctx context.Context, uid int64, limit int) ([]model.Conversation, error) {
	var list []model.Conversation
	err := r.db.WithContext(ctx).Where("user_id = ?", uid).
		Order("pinned DESC, last_msg_at DESC").Limit(limit).Find(&list).Error
	return list, err
}

// GetConversation 查询单个会话。
func (r *MessageRepo) GetConversation(ctx context.Context, uid int64, convID string) (*model.Conversation, error) {
	var c model.Conversation
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND conv_id = ?", uid, convID).Limit(1).Find(&c).Error
	if err != nil || c.ID == 0 {
		return nil, err
	}
	return &c, nil
}

// UpdateConversation 局部更新会话设置（置顶、免打扰、清空）。
func (r *MessageRepo) UpdateConversation(ctx context.Context, uid int64, convID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.Conversation{}).
		Where("user_id = ? AND conv_id = ?", uid, convID).Updates(fields).Error
}

// UpsertRead 更新已读位置，只允许前进不允许回退（GREATEST 保证幂等）。
func (r *MessageRepo) UpsertRead(ctx context.Context, uid int64, convID string, seq int64) error {
	row := model.ConversationRead{UserID: uid, ConvID: convID, LastReadSeq: seq}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "conv_id"}},
		DoUpdates: clause.Assignments(map[string]any{"last_read_seq": gorm.Expr("GREATEST(last_read_seq, ?)", seq)}),
	}).Create(&row).Error
}

// UpsertConversationSettings 写入用户的会话设置（置顶/免打扰）。
// 读扩散后群会话不再预建行，用户首次设置时才落库，因此这里用 UPSERT 而不是 UPDATE。
func (r *MessageRepo) UpsertConversationSettings(ctx context.Context, row model.Conversation) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "conv_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"pinned", "muted", "updated_at"}),
	}).Create(&row).Error
}

// ListReadSeqs 批量查询指定会话的已读位置，用于 Redis 未命中时回源。
func (r *MessageRepo) ListReadSeqs(ctx context.Context, uid int64, convIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	type row struct {
		ConvID      string
		LastReadSeq int64
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&model.ConversationRead{}).
		Select("conv_id, last_read_seq").
		Where("user_id = ? AND conv_id IN ?", uid, convIDs).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ConvID] = r.LastReadSeq
	}
	return out, nil
}

// GetReadSeq 读取已读位置。
func (r *MessageRepo) GetReadSeq(ctx context.Context, uid int64, convID string) (int64, error) {
	var row model.ConversationRead
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND conv_id = ?", uid, convID).Limit(1).Find(&row).Error
	return row.LastReadSeq, err
}

// Recall 撤回消息：仅更新状态，保留行以便客户端展示"已撤回"。
func (r *MessageRepo) Recall(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&model.Message{}).
		Where("id = ?", id).Update("status", model.MsgStatusRecalled).Error
}
