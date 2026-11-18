package model

import (
	"time"

	"imchat/internal/media"
)

// 群成员角色。
const (
	GroupRoleMember int8 = 0
	GroupRoleAdmin  int8 = 1
	GroupRoleOwner  int8 = 2
)

// 群状态。
const (
	GroupStatusNormal    int8 = 0
	GroupStatusDissolved int8 = 1
)

// MaxGroupMembers 是群成员上限。达到大群规模后采用「读扩散」：
// 发送消息只更新群这一行的最新消息指针，不为每个成员写会话行与未读计数，
// 写入代价从 O(成员数) 降为 O(1)。
const MaxGroupMembers = 1000

// Group 群聊。
// member_count 为冗余计数，避免列表页 count(*) 聚合；
// last_msg_* 是读扩散的关键载体：一条群消息只更新这一行，成员读取会话列表时据此组装。
type Group struct {
	BaseModel
	Name           string     `gorm:"size:64;not null" json:"name"`
	Avatar         string     `gorm:"size:255" json:"avatar"`
	OwnerID        int64      `gorm:"not null;index" json:"owner_id,string"`
	Notice         string     `gorm:"size:500" json:"notice"`
	MemberCount    int        `gorm:"not null;default:0" json:"member_count"`
	MaxMember      int        `gorm:"not null;default:1000" json:"max_member"`
	Status         int8       `gorm:"not null;default:0;index" json:"status"`
	LastMsgID      int64      `gorm:"not null;default:0" json:"last_msg_id,string"`
	LastMsgSeq     int64      `gorm:"not null;default:0" json:"last_msg_seq"`
	LastMsgPreview string     `gorm:"size:128" json:"last_msg_preview"`
	LastMsgType    int8       `gorm:"not null;default:0" json:"last_msg_type"`
	LastMsgAt      *time.Time `json:"last_msg_at,omitempty"`
}

// TableName 固定表名。
func (Group) TableName() string { return "groups" }

// AvatarURL 返回可直接访问的群头像地址。
//
// 库里存的是对象 key，出站才拼前缀：换 CDN 只改配置，不用回刷 groups.avatar。
// 与 User.ToPublic 同一口径，群相关的 DTO 都从这里取头像。
func (g *Group) AvatarURL() string { return media.URL(g.Avatar) }

// GroupMember 群成员。
type GroupMember struct {
	BaseModel
	GroupID       int64     `gorm:"not null;uniqueIndex:uk_group_user,priority:1" json:"group_id,string"`
	UserID        int64     `gorm:"not null;uniqueIndex:uk_group_user,priority:2;index:idx_user_joined,priority:1" json:"user_id,string"`
	Role          int8      `gorm:"not null;default:0" json:"role"`
	GroupNickname string    `gorm:"size:32" json:"group_nickname"`
	Muted         bool      `gorm:"not null;default:false" json:"muted"` // 被禁言
	JoinedAt      time.Time `gorm:"index:idx_user_joined,priority:2" json:"joined_at"`
}

// TableName 固定表名。
func (GroupMember) TableName() string { return "group_members" }

// IsAdmin 是否有管理权限（群主或管理员）。
func (m *GroupMember) IsAdmin() bool {
	return m.Role == GroupRoleAdmin || m.Role == GroupRoleOwner
}
