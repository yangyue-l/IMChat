// Package model 定义 GORM 数据模型。所有主键均为雪花 ID，JSON 序列化为字符串。
package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"imchat/internal/pkg/snowflake"
)

// BaseModel 提供雪花主键与时间戳；插入前自动分配 ID，避免业务层重复赋值。
//
// CreatedAt 故意不加 index：全库没有一条查询按 created_at 过滤或排序，
// 列表一律是「等值列 + 主键游标」（user_id + id、conv_id + seq、user_id + last_msg_at）。
// 12 张表各挂一个用不到的单列索引，换来的是每次写入多一次 B+ 树维护，
// 对 messages 这种写多读少的大表是纯损耗。
type BaseModel struct {
	ID        int64     `gorm:"primaryKey;autoIncrement:false" json:"id,string"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BeforeCreate 在落库前兜底生成 ID。
func (b *BaseModel) BeforeCreate(*gorm.DB) error {
	if b.ID == 0 {
		b.ID = snowflake.Next()
	}
	return nil
}

// StringList 以 JSON 文本形式存库，用于图片列表等简单数组字段。
type StringList []string

// Value 实现 driver.Valuer。
func (s StringList) Value() (driver.Value, error) {
	if s == nil {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("序列化字符串列表失败: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner。
func (s *StringList) Scan(src any) error {
	if src == nil {
		*s = StringList{}
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return errors.New("StringList 不支持的数据类型")
	}
	if len(raw) == 0 {
		*s = StringList{}
		return nil
	}
	return json.Unmarshal(raw, s)
}

// 会话类型。
const (
	ConvPrivate int8 = 1
	ConvGroup   int8 = 2
)

// 消息类型。
const (
	MsgText   int8 = 1
	MsgImage  int8 = 2
	MsgFile   int8 = 3
	MsgSystem int8 = 4
)

// 消息状态。
const (
	MsgStatusNormal   int8 = 0
	MsgStatusRecalled int8 = 1
)

// 朋友圈可见范围。
const (
	MomentPublic  int8 = 1 // 公开
	MomentFriends int8 = 2 // 仅好友
	MomentPrivate int8 = 3 // 仅自己
)
