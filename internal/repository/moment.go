package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
)

// MomentRepo 朋友圈数据访问。
type MomentRepo struct{ db *gorm.DB }

// NewMomentRepo 创建仓库。
func NewMomentRepo(db *gorm.DB) *MomentRepo { return &MomentRepo{db: db} }

// WithTx 返回绑定到事务的副本。点赞/评论与计数必须同事务，否则
// 「写成功、计数失败」会让 like_count 永久偏小，重试又被唯一索引判为重复，永远不会自愈。
func (r *MomentRepo) WithTx(tx *gorm.DB) *MomentRepo { return &MomentRepo{db: tx} }

// Create 发布动态。
func (r *MomentRepo) Create(ctx context.Context, m *model.Moment) error {
	return r.db.WithContext(ctx).Create(m).Error
}

// Get 查询单条动态。软删除后对外不可见，因此这里过滤 status，
// 避免已删除的动态还能被点赞、被评论。
func (r *MomentRepo) Get(ctx context.Context, id int64) (*model.Moment, error) {
	var m model.Moment
	err := r.db.WithContext(ctx).Where("id = ? AND status = ?", id, model.MomentStatusNormal).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &m, err
}

// ListByUsers 拉模式时间线：从指定作者集合中取动态，用 ID 游标避免深分页。
func (r *MomentRepo) ListByUsers(ctx context.Context, authorIDs []int64, beforeID int64, limit int) ([]model.Moment, error) {
	if len(authorIDs) == 0 {
		return nil, nil
	}
	var list []model.Moment
	q := r.db.WithContext(ctx).
		Where("user_id IN ? AND status = ?", authorIDs, model.MomentStatusNormal)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	err := q.Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// ListByIDs 按 ID 集合批量取动态（时间线缓存命中后回表）。
func (r *MomentRepo) ListByIDs(ctx context.Context, ids []int64) ([]model.Moment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var list []model.Moment
	err := r.db.WithContext(ctx).Where("id IN ? AND status = ?", ids, model.MomentStatusNormal).Find(&list).Error
	return list, err
}

// ListByAuthor 查询某个用户的动态（他人主页），可见性由 service 过滤。
func (r *MomentRepo) ListByAuthor(ctx context.Context, authorID, beforeID int64, limit int) ([]model.Moment, error) {
	var list []model.Moment
	q := r.db.WithContext(ctx).Where("user_id = ? AND status = ?", authorID, model.MomentStatusNormal)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	err := q.Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// Delete 软删除：仅置状态，便于审计与恢复。
func (r *MomentRepo) Delete(ctx context.Context, id, authorID int64) error {
	return r.db.WithContext(ctx).Model(&model.Moment{}).
		Where("id = ? AND user_id = ?", id, authorID).Update("status", model.MomentStatusDeleted).Error
}

// IncrCounters 原子调整点赞/评论计数。
func (r *MomentRepo) IncrCounters(ctx context.Context, id int64, likeDelta, commentDelta int) error {
	return r.db.WithContext(ctx).Model(&model.Moment{}).Where("id = ?", id).
		UpdateColumns(map[string]any{
			"like_count":    gorm.Expr("GREATEST(like_count + ?, 0)", likeDelta),
			"comment_count": gorm.Expr("GREATEST(comment_count + ?, 0)", commentDelta),
		}).Error
}

// AddLike 点赞，重复点赞由唯一索引兜住。
func (r *MomentRepo) AddLike(ctx context.Context, momentID, uid int64) (bool, error) {
	row := model.MomentLike{MomentID: momentID, UserID: uid}
	res := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return res.RowsAffected > 0, res.Error
}

// DelLike 取消点赞。
func (r *MomentRepo) DelLike(ctx context.Context, momentID, uid int64) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("moment_id = ? AND user_id = ?", momentID, uid).
		Delete(&model.MomentLike{})
	return res.RowsAffected > 0, res.Error
}

// LikeUserIDs 批量查询某批动态的点赞者，避免 N+1。
func (r *MomentRepo) LikeUserIDs(ctx context.Context, momentIDs []int64) (map[int64][]int64, error) {
	if len(momentIDs) == 0 {
		return map[int64][]int64{}, nil
	}
	type row struct {
		MomentID int64
		UserID   int64
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&model.MomentLike{}).
		Select("moment_id, user_id").Where("moment_id IN ?", momentIDs).
		Order("id ASC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(momentIDs))
	for _, r := range rows {
		out[r.MomentID] = append(out[r.MomentID], r.UserID)
	}
	return out, nil
}

// AddComment 新增评论。
func (r *MomentRepo) AddComment(ctx context.Context, c *model.MomentComment) error {
	return r.db.WithContext(ctx).Create(c).Error
}

// ListComments 查询动态下的评论。
func (r *MomentRepo) ListComments(ctx context.Context, momentID int64, limit int) ([]model.MomentComment, error) {
	var list []model.MomentComment
	err := r.db.WithContext(ctx).Where("moment_id = ?", momentID).
		Order("id ASC").Limit(limit).Find(&list).Error
	return list, err
}

// CommentsByMoments 批量查询评论，避免 N+1。
func (r *MomentRepo) CommentsByMoments(ctx context.Context, momentIDs []int64, perMoment int) (map[int64][]model.MomentComment, error) {
	if len(momentIDs) == 0 {
		return map[int64][]model.MomentComment{}, nil
	}
	var list []model.MomentComment
	err := r.db.WithContext(ctx).Where("moment_id IN ?", momentIDs).
		Order("id ASC").Limit(len(momentIDs) * perMoment).Find(&list).Error
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]model.MomentComment, len(momentIDs))
	for _, c := range list {
		if len(out[c.MomentID]) >= perMoment {
			continue
		}
		out[c.MomentID] = append(out[c.MomentID], c)
	}
	return out, nil
}

// GetComment 查询单条评论。
func (r *MomentRepo) GetComment(ctx context.Context, id int64) (*model.MomentComment, error) {
	var c model.MomentComment
	err := r.db.WithContext(ctx).First(&c, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}

// DelComment 删除单条评论。
func (r *MomentRepo) DelComment(ctx context.Context, id, uid int64) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", id, uid).Delete(&model.MomentComment{})
	return res.RowsAffected > 0, res.Error
}

// DelCommentReplies 删除挂在某条评论下的全部回复，返回实际删除条数。
//
// 不带 user_id 条件：一级评论作者删掉自己的评论后，别人挂在它下面的回复就没有
// 上下文了，留着只会渲染成一串没有归属的碎片，所以整棵子树一起删。
func (r *MomentRepo) DelCommentReplies(ctx context.Context, parentID int64) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("parent_id = ?", parentID).Delete(&model.MomentComment{})
	return res.RowsAffected, res.Error
}
