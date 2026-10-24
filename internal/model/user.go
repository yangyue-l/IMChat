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
	Username     string `gorm:"size:32;uniqueIndex;not null" json:"username"`
	PasswordHash string `gorm:"size:100;not null" json:"-"`
	Nickname     string `gorm:"size:32;not null" json:"nickname"`
	Avatar       string `gorm:"size:255" json:"avatar"`
	Gender       int8   `gorm:"not null;default:0" json:"gender"` // 0 未知 1 男 2 女
	Signature    string `gorm:"size:255" json:"signature"`

	// Phone 预留的手机号，目前没有任何读写路径。
	//
	// 用指针 + 唯一索引而不是 string + 普通索引：未绑定是「没有值」，不是空串。
	// 唯一索引下多个 NULL 合法、多个空串非法，改成指针后将来接入手机号登录
	// 可以直接落在唯一索引上，不需要再洗一次数据。
	Phone *string `gorm:"size:20;uniqueIndex:uk_users_phone" json:"-"`

	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	Status       int8       `gorm:"not null;default:0;index" json:"status"`
}

// TableName 固定表名，避免后续重命名造成迁移歧义。
func (User) TableName() string { return "users" }

// PublicUser 是对外暴露的用户信息，剔除密码等敏感字段。
//
// 故意不含 Username：登录名等同于「QQ 号」，不该出现在好友列表、群成员、
// 消息发送者、点赞/评论者这些对所有人可见的投影里。只在主动搜索时回传，
// 见 service.UserSearchItem。
type PublicUser struct {
	ID        int64  `json:"id,string"`
	Nickname  string `json:"nickname"`
	Avatar    string `json:"avatar"`
	Gender    int8   `json:"gender"`
	Signature string `json:"signature"`
}

// ToPublic 转换为对外结构。
func (u *User) ToPublic() PublicUser {
	return PublicUser{
		ID:        u.ID,
		Nickname:  u.Nickname,
		Avatar:    u.Avatar,
		Gender:    u.Gender,
		Signature: u.Signature,
	}
}
