package model

// 动态状态。删除走软删，与 message 的 status 语义保持一致。
const (
	MomentStatusNormal  int8 = 0
	MomentStatusDeleted int8 = 1
)

// Moment 朋友圈动态。likes/comments 计数冗余在动态行上，列表页不必聚合。
type Moment struct {
	BaseModel
	UserID       int64      `gorm:"not null;index:idx_user_created,priority:1" json:"user_id,string"`
	Content      string     `gorm:"type:text" json:"content"`
	Images       StringList `gorm:"type:text" json:"images"`
	Visibility   int8       `gorm:"not null;default:2" json:"visibility"` // 1 公开 2 仅好友 3 私密
	Location     string     `gorm:"size:64" json:"location"`
	LikeCount    int        `gorm:"not null;default:0" json:"like_count"`
	CommentCount int        `gorm:"not null;default:0" json:"comment_count"`
	Status       int8       `gorm:"not null;default:0;index" json:"status"`
}

// TableName 固定表名。
func (Moment) TableName() string { return "moments" }

// MomentLike 点赞记录。
type MomentLike struct {
	BaseModel
	MomentID int64 `gorm:"not null;uniqueIndex:uk_moment_user,priority:1" json:"moment_id,string"`
	UserID   int64 `gorm:"not null;uniqueIndex:uk_moment_user,priority:2" json:"user_id,string"`
}

// TableName 固定表名。
func (MomentLike) TableName() string { return "moment_likes" }

// MomentComment 评论。ParentID 支持二级回复，ReplyToUserID 用于 @ 提示展示。
type MomentComment struct {
	BaseModel
	MomentID      int64  `gorm:"not null;index:idx_moment_created,priority:1" json:"moment_id,string"`
	UserID        int64  `gorm:"not null" json:"user_id,string"`
	ParentID      int64  `gorm:"not null;default:0" json:"parent_id,string"`
	ReplyToUserID int64  `gorm:"not null;default:0" json:"reply_to_user_id,string"`
	Content       string `gorm:"size:500;not null" json:"content"`
}

// TableName 固定表名。
func (MomentComment) TableName() string { return "moment_comments" }

// AllModels 返回全部需要迁移的模型，供 AutoMigrate 使用。
func AllModels() []any {
	return []any{
		&User{},
		&Friend{},
		&FriendRequest{},
		&Blacklist{},
		&Group{},
		&GroupMember{},
		&Message{},
		&Conversation{},
		&ConversationRead{},
		&Moment{},
		&MomentLike{},
		&MomentComment{},
	}
}
