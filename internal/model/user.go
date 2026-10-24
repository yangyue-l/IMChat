package model

import "time"

// UserStatus 账号状态。
const (
	UserStatusNormal   int8 = 0
	UserStatusDisabled int8 = 1
)

// User 用户表。username 唯一且用于登录，nickname 仅展示。
type User struct {
	BaseModel
	Username     string     `gorm:"size:32;uniqueIndex;not null" json:"username"`
	PasswordHash string     `gorm:"size:100;not null" json:"-"`
	Nickname     string     `gorm:"size:32;not null" json:"nickname"`
	Avatar       string     `gorm:"size:255" json:"avatar"`
	Gender       int8       `gorm:"not null;default:0" json:"gender"` // 0 未知 1 男 2 女
	Signature    string     `gorm:"size:255" json:"signature"`
	Phone        string     `gorm:"size:20;index" json:"phone,omitempty"`
	Status       int8       `gorm:"not null;default:0;index" json:"status"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
}

// TableName 固定表名，避免后续重命名造成迁移歧义。
func (User) TableName() string { return "users" }

// PublicUser 是对外暴露的用户信息，剔除密码等敏感字段。
type PublicUser struct {
	ID        int64  `json:"id,string"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	Avatar    string `json:"avatar"`
	Gender    int8   `json:"gender"`
	Signature string `json:"signature"`
}

// ToPublic 转换为对外结构。
func (u *User) ToPublic() PublicUser {
	return PublicUser{
		ID:        u.ID,
		Username:  u.Username,
		Nickname:  u.Nickname,
		Avatar:    u.Avatar,
		Gender:    u.Gender,
		Signature: u.Signature,
	}
}
