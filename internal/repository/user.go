// Package repository 封装数据访问，隔离 GORM 细节，便于替换存储与单测。
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"imchat/internal/model"
)

// UserRepo 用户数据访问。
type UserRepo struct{ db *gorm.DB }

// NewUserRepo 创建仓库。
func NewUserRepo(db *gorm.DB) *UserRepo { return &UserRepo{db: db} }

// Create 新增用户。
func (r *UserRepo) Create(ctx context.Context, u *model.User) error {
	return r.db.WithContext(ctx).Create(u).Error
}

// GetByID 按主键查询。
func (r *UserRepo) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).First(&u, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &u, err
}

// GetByUsername 按用户名查询，登录与注册查重使用。
func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).First(&u, "username = ?", username).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &u, err
}

// GetByIDs 批量查询，用于消息列表补全发送者信息。
func (r *UserRepo) GetByIDs(ctx context.Context, ids []int64) ([]model.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var list []model.User
	err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&list).Error
	return list, err
}

// Search 按昵称或用户名模糊搜索，前缀匹配可命中索引；limit 强制收敛结果集。
func (r *UserRepo) Search(ctx context.Context, keyword string, limit int) ([]model.User, error) {
	var list []model.User
	err := r.db.WithContext(ctx).
		Where("username LIKE ? OR nickname LIKE ?", keyword+"%", keyword+"%").
		Order("id DESC").Limit(limit).Find(&list).Error
	return list, err
}

// UpdateFields 局部更新，避免整行覆盖造成写放大。
func (r *UserRepo) UpdateFields(ctx context.Context, id int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", id).Updates(fields).Error
}
