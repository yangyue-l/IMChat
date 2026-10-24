package model

// 好友申请状态。
const (
	FriendRequestPending  int8 = 0
	FriendRequestAccepted int8 = 1
	FriendRequestRejected int8 = 2
)

// Friend 好友关系。双向各存一行，保证「我的好友列表」可走单索引范围扫描。
type Friend struct {
	BaseModel
	UserID   int64  `gorm:"not null;uniqueIndex:uk_user_friend,priority:1" json:"user_id,string"`
	FriendID int64  `gorm:"not null;uniqueIndex:uk_user_friend,priority:2" json:"friend_id,string"`
	Remark   string `gorm:"size:32" json:"remark"` // 好友备注
	Starred  bool   `gorm:"not null;default:false" json:"starred"`
}

// TableName 固定表名。
func (Friend) TableName() string { return "friends" }

// FriendRequest 好友申请。status 变化后保留历史，便于审计。
type FriendRequest struct {
	BaseModel
	FromUserID int64  `gorm:"not null;index:idx_from_to,priority:1" json:"from_user_id,string"`
	ToUserID   int64  `gorm:"not null;index:idx_to_status,priority:1;index:idx_from_to,priority:2" json:"to_user_id,string"`
	Message    string `gorm:"size:255" json:"message"`
	Status     int8   `gorm:"not null;default:0;index:idx_to_status,priority:2" json:"status"`
}

// TableName 固定表名。
func (FriendRequest) TableName() string { return "friend_requests" }

// Blacklist 黑名单。被拉黑方发消息会被拒绝，且互相看不到朋友圈动态。
type Blacklist struct {
	BaseModel
	UserID        int64 `gorm:"not null;uniqueIndex:uk_user_blocked,priority:1" json:"user_id,string"`
	BlockedUserID int64 `gorm:"not null;uniqueIndex:uk_user_blocked,priority:2;index" json:"blocked_user_id,string"`
}

// TableName 固定表名。
func (Blacklist) TableName() string { return "blacklists" }
