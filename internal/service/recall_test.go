package service

import (
	"testing"
	"time"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// TestCanRecall 覆盖撤回的四类判定：自己的消息按窗口、他人消息按角色、
// 私聊不认管理员、窗口缺省时兜底。
//
// 这些分支决定「谁能让一条已发出的消息消失」，任何一条写松了都是权限漏洞：
// 窗口写松 → 隔天也能撤回；角色写松 → 普通成员能删别人的话。
func TestCanRecall(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	const window = 2 * time.Minute

	// 每条用例只关心「多老的消息、什么会话、谁发的」，用构造器省掉重复填充。
	msg := func(convType int8, fromID int64, age time.Duration) *model.Message {
		m := &model.Message{ConvType: convType, FromID: fromID, GroupID: 7, ToID: 2}
		m.CreatedAt = now.Add(-age)
		return m
	}
	admin := &model.GroupMember{Role: model.GroupRoleAdmin}
	owner := &model.GroupMember{Role: model.GroupRoleOwner}
	plain := &model.GroupMember{Role: model.GroupRoleMember}

	cases := []struct {
		name     string
		msg      *model.Message
		uid      int64
		member   *model.GroupMember
		window   time.Duration
		wantCode int
	}{
		{"自己在窗内可撤回", msg(model.ConvPrivate, 1, time.Minute), 1, nil, window, 0},
		{"自己恰好在窗口边界放行", msg(model.ConvPrivate, 1, window), 1, nil, window, 0},
		{"自己超出窗口被拒", msg(model.ConvPrivate, 1, window+time.Second), 1, nil, window, resp.CodeForbidden},
		{"群聊里撤回自己的消息也受窗口约束", msg(model.ConvGroup, 1, 10*time.Minute), 1, nil, window, resp.CodeForbidden},
		{"私聊撤回他人消息被拒", msg(model.ConvPrivate, 2, time.Minute), 1, nil, window, resp.CodeForbidden},
		{"群里普通成员撤回他人消息被拒", msg(model.ConvGroup, 2, time.Minute), 1, plain, window, resp.CodeForbidden},
		{"群里查不到成员记录被拒", msg(model.ConvGroup, 2, time.Minute), 1, nil, window, resp.CodeForbidden},
		{"群管理员可撤回他人消息", msg(model.ConvGroup, 2, time.Minute), 1, admin, window, 0},
		{"群主可撤回他人消息", msg(model.ConvGroup, 2, time.Minute), 1, owner, window, 0},
		{"管理员撤回超窗的他人消息仍放行", msg(model.ConvGroup, 2, time.Hour), 1, admin, window, 0},
		{"窗口缺省时按 2 分钟兜底", msg(model.ConvPrivate, 1, time.Minute), 1, nil, 0, 0},
		{"窗口缺省时超 2 分钟仍被拒", msg(model.ConvPrivate, 1, 3*time.Minute), 1, nil, 0, resp.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := canRecall(c.msg, c.uid, c.member, c.window, now)
			if c.wantCode == 0 {
				if err != nil {
					t.Fatalf("应放行，实际 %v", err)
				}
				return
			}
			if got := errCode(err); got != c.wantCode {
				t.Fatalf("errCode = %d，期望 %d（err=%v）", got, c.wantCode, err)
			}
		})
	}
}
