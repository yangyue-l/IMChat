package model

import "time"

// Message 统一消息表。私聊与群聊共用，靠 ConvID 区分：
// 私聊 conv_id = "p_<小uid>_<大uid>"，群聊 conv_id = "g_<gid>"。
// Seq 由 Redis 按会话自增，客户端可据此判断消息是否断档。
//
// 嵌入 BaseModel 而不是自己重复声明 ID/CreatedAt：否则 BeforeCreate 兜底失效，
// 每个新写入路径都得手动分配雪花 ID；且撤回时没有 UpdatedAt 记录操作时间。
type Message struct {
	BaseModel
	ConvID      string `gorm:"size:64;not null;index:idx_conv_seq,priority:1" json:"conv_id"`
	ConvType    int8   `gorm:"not null" json:"conv_type"`
	Seq         int64  `gorm:"not null;index:idx_conv_seq,priority:2" json:"seq"`
	FromID      int64  `gorm:"not null;index" json:"from_id,string"`
	ToID        int64  `gorm:"not null;default:0" json:"to_id,string"` // 私聊接收方，群聊为 0
	GroupID     int64  `gorm:"not null;default:0;index" json:"group_id,string"`
	ClientMsgID string `gorm:"size:64;not null;uniqueIndex:uk_from_client" json:"client_msg_id"`
	MsgType     int8   `gorm:"not null;default:1" json:"msg_type"`
	Content     string `gorm:"type:text" json:"content"`
	Extra       string `gorm:"type:varchar(1024)" json:"extra,omitempty"` // 图片宽高、文件名等扩展信息
	Status      int8   `gorm:"not null;default:0" json:"status"`
}

// TableName 固定表名。
func (Message) TableName() string { return "messages" }

// Conversation 用户会话列表。每个参与者各一行，冗余最后一条消息避免列表页回表。
// idx_user_last 必须把 user_id 放在首列：列表查询是「按用户过滤 + 按时间排序」，
// 只建 last_msg_at 单列索引时过滤条件用不上它，等于白交索引维护成本。
type Conversation struct {
	BaseModel
	UserID         int64     `gorm:"not null;uniqueIndex:uk_user_conv,priority:1;index:idx_user_last,priority:1" json:"user_id,string"`
	ConvID         string    `gorm:"size:64;not null;uniqueIndex:uk_user_conv,priority:2" json:"conv_id"`
	ConvType       int8      `gorm:"not null" json:"conv_type"`
	TargetID       int64     `gorm:"not null" json:"target_id,string"` // 私聊为对方 uid，群聊为 gid
	LastMsgID      int64     `gorm:"not null;default:0" json:"last_msg_id,string"`
	LastMsgPreview string    `gorm:"size:128" json:"last_msg_preview"`
	LastMsgType    int8      `gorm:"not null;default:0" json:"last_msg_type"`
	LastMsgAt      time.Time `gorm:"index:idx_user_last,priority:2" json:"last_msg_at"`
	Pinned         bool      `gorm:"not null;default:false" json:"pinned"`
	Muted          bool      `gorm:"not null;default:false" json:"muted"`
}

// TableName 固定表名。
func (Conversation) TableName() string { return "conversations" }

// ConversationRead 记录用户在会话中的已读位置，用于未读数计算与多端同步已读。
type ConversationRead struct {
	BaseModel
	UserID      int64  `gorm:"not null;uniqueIndex:uk_user_conv,priority:1" json:"user_id,string"`
	ConvID      string `gorm:"size:64;not null;uniqueIndex:uk_user_conv,priority:2" json:"conv_id"`
	LastReadSeq int64  `gorm:"not null;default:0" json:"last_read_seq"`
}

// TableName 固定表名。
func (ConversationRead) TableName() string { return "conversation_reads" }
