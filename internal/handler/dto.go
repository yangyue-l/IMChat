package handler

import "imchat/internal/pkg/jsonx"

// 本文件只放「传输层语义」的请求体：开关字段、仅做透传的字段、handler 拆完就丢的参数。
//
// 归属约定：
//   - 承载业务校验、会被 service 直接消费的入参，定义在 service（如 service.RegisterReq）；
//   - 只有 HTTP 外形、handler 解析后即拆散的入参，定义在这里。
//
// 不要在 handler 里写内联 struct：同一形状重复出现时，改一处漏一处不会有编译期提示。

// UserTargetReq 只携带目标用户 id 的请求体，拉黑、转让等接口共用。
// 前端 JS 的大整数会序列化成字符串，用 `,string` 接收，避免精度丢失。
type UserTargetReq struct {
	UserID int64 `json:"user_id,string"`
}

// UpdateFriendReq 修改好友备注与星标。
// 指针用于区分「未传」与「传零值」，未传时 service 不覆盖原值。
type UpdateFriendReq struct {
	Remark  *string `json:"remark"`
	Starred *bool   `json:"starred"`
}

// HandleFriendRequestReq 处理好友申请。
// Accept 必须用指针：旧实现是 bool，客户端漏传时取零值 false，
// 会把「同意」静默变成「拒绝」，因此由 handler 显式校验非 nil。
type HandleFriendRequestReq struct {
	Accept *bool `json:"accept"`
}

// UpdateConversationReq 设置会话置顶与免打扰，指针语义同 UpdateFriendReq。
type UpdateConversationReq struct {
	Pinned *bool `json:"pinned"`
	Muted  *bool `json:"muted"`
}

// MarkReadReq 上报已读位置。
type MarkReadReq struct {
	ConvID string `json:"conv_id"`
	Seq    int64  `json:"seq"`
}

// AddGroupMembersReq 邀请成员入群。
type AddGroupMembersReq struct {
	UserIDs jsonx.Int64List `json:"user_ids"`
}

// MuteMemberReq 设置群成员禁言。
type MuteMemberReq struct {
	UserTargetReq
	Muted bool `json:"muted"`
}

// SetGroupAdminReq 设置或取消群管理员。
type SetGroupAdminReq struct {
	UserTargetReq
	Admin bool `json:"admin"`
}

// AddCommentReq 发表评论。
type AddCommentReq struct {
	Content  string `json:"content"`
	ReplyTo  int64  `json:"reply_to,string"`
	ParentID int64  `json:"parent_id,string"`
}
