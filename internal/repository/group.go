package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
)

// GroupRepo 群与群成员数据访问。
type GroupRepo struct{ db *gorm.DB }

// NewGroupRepo 创建仓库。
func NewGroupRepo(db *gorm.DB) *GroupRepo { return &GroupRepo{db: db} }

// DB 暴露事务句柄，供 service 组装跨表事务。
func (r *GroupRepo) DB() *gorm.DB { return r.db }

// WithTx 返回绑定到事务的副本，供 service 组装跨表原子写。
func (r *GroupRepo) WithTx(tx *gorm.DB) *GroupRepo { return &GroupRepo{db: tx} }

// Create 创建群。
func (r *GroupRepo) Create(ctx context.Context, g *model.Group) error {
	return r.db.WithContext(ctx).Create(g).Error
}

// Get 按 ID 查询群。
func (r *GroupRepo) Get(ctx context.Context, id int64) (*model.Group, error) {
	var g model.Group
	err := r.db.WithContext(ctx).First(&g, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &g, err
}

// GetMany 批量查询群资料。
func (r *GroupRepo) GetMany(ctx context.Context, ids []int64) ([]model.Group, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var list []model.Group
	err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&list).Error
	return list, err
}

// UpdateFields 局部更新群资料。
func (r *GroupRepo) UpdateFields(ctx context.Context, id int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", id).Updates(fields).Error
}

// AddMembers 批量加群，重复成员忽略。
func (r *GroupRepo) AddMembers(ctx context.Context, members []model.GroupMember) (int64, error) {
	if len(members) == 0 {
		return 0, nil
	}
	res := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&members)
	return res.RowsAffected, res.Error
}

// GetMember 查询单个成员。
func (r *GroupRepo) GetMember(ctx context.Context, gid, uid int64) (*model.GroupMember, error) {
	var m model.GroupMember
	err := r.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", gid, uid).Limit(1).Find(&m).Error
	if err != nil || m.ID == 0 {
		return nil, err
	}
	return &m, nil
}

// ListMembers 查询群成员。
func (r *GroupRepo) ListMembers(ctx context.Context, gid int64) ([]model.GroupMember, error) {
	var list []model.GroupMember
	err := r.db.WithContext(ctx).Where("group_id = ?", gid).
		Order("role DESC, id ASC").Find(&list).Error
	return list, err
}

// MemberIDs 只取成员 ID，用于消息扇出。
func (r *GroupRepo) MemberIDs(ctx context.Context, gid int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Model(&model.GroupMember{}).
		Where("group_id = ?", gid).Pluck("user_id", &ids).Error
	return ids, err
}

// RemoveMember 移除成员。
func (r *GroupRepo) RemoveMember(ctx context.Context, gid, uid int64) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", gid, uid).
		Delete(&model.GroupMember{})
	return res.RowsAffected, res.Error
}

// UpdateMember 更新成员属性（角色、禁言、群昵称）。
func (r *GroupRepo) UpdateMember(ctx context.Context, gid, uid int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.GroupMember{}).
		Where("group_id = ? AND user_id = ?", gid, uid).Updates(fields).Error
}

// IncrMemberCount 原子增减群人数，避免读改写竞态。
func (r *GroupRepo) IncrMemberCount(ctx context.Context, gid int64, delta int) error {
	return r.db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", gid).
		UpdateColumn("member_count", gorm.Expr("GREATEST(member_count + ?, 0)", delta)).Error
}

// UpdateLastMessage 更新群的最新消息指针。
// 这是读扩散的核心写操作：一条群消息无论群里有 3 人还是 1000 人，都只写这一行。
//
// 必须带 last_msg_seq 前进条件：并发发送时事务提交顺序可能与该 seq 顺序相反，
// 无条件覆盖会让指针倒退回较小的 seq，而读扩散的未读数是按 seq 差算的 ——
// 指针一倒退，整个群所有成员的未读数都会错。
func (r *GroupRepo) UpdateLastMessage(ctx context.Context, gid int64, msg *model.Message, preview string, at time.Time) error {
	return r.db.WithContext(ctx).Model(&model.Group{}).
		Where("id = ? AND last_msg_seq < ?", gid, msg.Seq).
		UpdateColumns(map[string]any{
			"last_msg_id":      msg.ID,
			"last_msg_seq":     msg.Seq,
			"last_msg_preview": preview,
			"last_msg_type":    msg.MsgType,
			"last_msg_at":      at,
		}).Error
}

// LockGroup 在事务内加行锁读取群。成员数上限这类「先校验后写」的操作必须持锁，
// 否则两个并发邀请都会读到 980 人、都判定不超限，最后写进 1020 人。
func (r *GroupRepo) LockGroup(ctx context.Context, gid int64) (*model.Group, error) {
	var g model.Group
	err := r.db.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&g, "id = ?", gid).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ExistingMemberIDs 从候选 ID 中筛出已在群内的部分，用于计算真正新增人数。
func (r *GroupRepo) ExistingMemberIDs(ctx context.Context, gid int64, ids []int64) (map[int64]struct{}, error) {
	out := make(map[int64]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var got []int64
	err := r.db.WithContext(ctx).Model(&model.GroupMember{}).
		Where("group_id = ? AND user_id IN ?", gid, ids).Pluck("user_id", &got).Error
	if err != nil {
		return nil, err
	}
	for _, id := range got {
		out[id] = struct{}{}
	}
	return out, nil
}

// SetMemberCount 直接把人数写成指定值，仅在持有群行锁的事务内使用。
func (r *GroupRepo) SetMemberCount(ctx context.Context, gid int64, count int) error {
	return r.db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", gid).
		UpdateColumn("member_count", count).Error
}

// ListUserGroups 查询用户加入的群（分页，走 idx_user_joined 索引）。
func (r *GroupRepo) ListUserGroups(ctx context.Context, uid int64, limit int) ([]model.GroupMember, error) {
	var list []model.GroupMember
	err := r.db.WithContext(ctx).Where("user_id = ?", uid).
		Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// ListAllMemberGroups 用于会话列表补全群资料。
func (r *GroupRepo) ListAllMemberGroups(ctx context.Context, uid int64) ([]model.GroupMember, error) {
	var list []model.GroupMember
	err := r.db.WithContext(ctx).Where("user_id = ?", uid).Find(&list).Error
	return list, err
}
