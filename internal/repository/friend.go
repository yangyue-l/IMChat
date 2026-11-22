package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
)

// FriendRepo 好友与黑名单数据访问。
type FriendRepo struct{ db *gorm.DB }

// NewFriendRepo 创建仓库。
func NewFriendRepo(db *gorm.DB) *FriendRepo { return &FriendRepo{db: db} }

// CreateRequest 创建好友申请。
func (r *FriendRepo) CreateRequest(ctx context.Context, req *model.FriendRequest) error {
	return r.db.WithContext(ctx).Create(req).Error
}

// FindPendingRequest 查找两人之间待处理的申请（双向），用于去重。
func (r *FriendRepo) FindPendingRequest(ctx context.Context, fromID, toID int64) (*model.FriendRequest, error) {
	var req model.FriendRequest
	err := r.db.WithContext(ctx).
		Where("from_user_id = ? AND to_user_id = ? AND status = ?", fromID, toID, model.FriendRequestPending).
		Order("id DESC").Limit(1).Find(&req).Error
	if err != nil || req.ID == 0 {
		return nil, err
	}
	return &req, nil
}

// GetRequest 按 ID 查询申请。
func (r *FriendRepo) GetRequest(ctx context.Context, id int64) (*model.FriendRequest, error) {
	var req model.FriendRequest
	err := r.db.WithContext(ctx).First(&req, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &req, err
}

// ListRequests 分页查询收到/发出的申请。
func (r *FriendRepo) ListRequests(ctx context.Context, uid int64, incoming bool, limit int) ([]model.FriendRequest, error) {
	var list []model.FriendRequest
	q := r.db.WithContext(ctx)
	if incoming {
		q = q.Where("to_user_id = ?", uid)
	} else {
		q = q.Where("from_user_id = ?", uid)
	}
	err := q.Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// DelFriends 双向删除好友关系。
func (r *FriendRepo) DelFriends(ctx context.Context, a, b int64) error {
	return r.db.WithContext(ctx).
		Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", a, b, b, a).
		Delete(&model.Friend{}).Error
}

// IsFriend 判断是否为好友，命中唯一索引。
func (r *FriendRepo) IsFriend(ctx context.Context, a, b int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Friend{}).
		Where("user_id = ? AND friend_id = ?", a, b).Limit(1).Count(&count).Error
	return count > 0, err
}

// ListFriends 查询好友关系行。
func (r *FriendRepo) ListFriends(ctx context.Context, uid int64) ([]model.Friend, error) {
	var list []model.Friend
	err := r.db.WithContext(ctx).Where("user_id = ?", uid).
		Order("starred DESC, id ASC").Find(&list).Error
	return list, err
}

// FriendIDs 只取好友 ID，供缓存与消息扇出使用。
func (r *FriendRepo) FriendIDs(ctx context.Context, uid int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Model(&model.Friend{}).
		Where("user_id = ?", uid).Pluck("friend_id", &ids).Error
	return ids, err
}

// UpdateFriend 更新备注与星标。
func (r *FriendRepo) UpdateFriend(ctx context.Context, uid, friendID int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.Friend{}).
		Where("user_id = ? AND friend_id = ?", uid, friendID).Updates(fields).Error
}

// Block 加入黑名单，重复拉黑忽略。
func (r *FriendRepo) Block(ctx context.Context, uid, targetID int64) error {
	row := model.Blacklist{UserID: uid, BlockedUserID: targetID}
	return r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// Unblock 移出黑名单。
func (r *FriendRepo) Unblock(ctx context.Context, uid, targetID int64) error {
	return r.db.WithContext(ctx).
		Where("user_id = ? AND blocked_user_id = ?", uid, targetID).
		Delete(&model.Blacklist{}).Error
}

// EitherBlocked 判断双方是否存在任一方向的拉黑关系。
func (r *FriendRepo) EitherBlocked(ctx context.Context, a, b int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.Blacklist{}).
		Where("(user_id = ? AND blocked_user_id = ?) OR (user_id = ? AND blocked_user_id = ?)", a, b, b, a).
		Limit(1).Count(&count).Error
	return count > 0, err
}

// ListBlocked 黑名单列表。
func (r *FriendRepo) ListBlocked(ctx context.Context, uid int64) ([]model.Blacklist, error) {
	var list []model.Blacklist
	err := r.db.WithContext(ctx).Where("user_id = ?", uid).
		Order("id DESC").Find(&list).Error
	return list, err
}

// BlockedRelatedIDs 取与 uid 存在任一方向拉黑关系的用户 ID（含我拉黑的、拉黑我的）。
// 朋友圈可见性必须双向判断，否则「我拉黑了对方」仍会被对方看到我的动态。
func (r *FriendRepo) BlockedRelatedIDs(ctx context.Context, uid int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Model(&model.Blacklist{}).
		Where("user_id = ? OR blocked_user_id = ?", uid, uid).
		Select("CASE WHEN user_id = ? THEN blocked_user_id ELSE user_id END", uid).
		Pluck("CASE WHEN user_id = ? THEN blocked_user_id ELSE user_id END", &ids).Error
	return ids, err
}
