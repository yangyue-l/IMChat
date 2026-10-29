package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// errCode 取出业务错误码，非 *resp.Error 返回 -1。
func errCode(err error) int {
	var ae *resp.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return -1
}

// TestNormalizePage 锁住分页归一化的四条边界。
//
// 这几条不是「顺手 clamp」，各自对应一个真实故障：
// 负 offset 进 SQL 会让 MySQL 直接报语法错误（500 而不是 400）；
// limit=0 若不兜底，接口会返回空列表，前端看起来像「群里没人」；
// 不设上限则一个请求就能把 1000 人的群整表拉走。
func TestNormalizePage(t *testing.T) {
	cases := []struct {
		name           string
		offset, limit  int
		wantOff, wantL int
	}{
		{"都缺省", 0, 0, 0, defaultMemberPageSize},
		{"正常范围原样返回", 100, 30, 100, 30},
		{"limit 负值当没传", 0, -5, 0, defaultMemberPageSize},
		{"limit 超上限被压到 200", 0, 5000, 0, maxMemberPageSize},
		{"limit 恰好等于上限", 0, maxMemberPageSize, 0, maxMemberPageSize},
		{"offset 负值归零", -3, 20, 0, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off, lim := normalizePage(c.offset, c.limit)
			if off != c.wantOff || lim != c.wantL {
				t.Fatalf("normalizePage(%d,%d) = (%d,%d)，期望 (%d,%d)",
					c.offset, c.limit, off, lim, c.wantOff, c.wantL)
			}
		})
	}
}

// TestMembersRejectsNonPositiveIDs 验证非法 uid/gid 在查库之前就被拦掉。
//
// deps 全空：只要代码顺序反了（先 mustBeMember 再校验参数），
// 就会因为 Groups 为 nil 而 panic —— 反过来证明校验确实在最前面。
func TestMembersRejectsNonPositiveIDs(t *testing.T) {
	svc := &GroupService{}
	cases := []struct{ uid, gid int64 }{
		{0, 1},
		{1, 0},
		{-7, 3},
		{3, -7},
	}
	for _, c := range cases {
		if _, err := svc.Members(context.Background(), c.uid, c.gid, 0, 0); errCode(err) != resp.CodeInvalidParam {
			t.Fatalf("uid=%d gid=%d 应返回参数错误，实际 %v", c.uid, c.gid, err)
		}
	}
}

// TestBuildMemberItemsKeepsOrderAndSkipsMissingUser 验证组装规则。
//
// 顺序必须跟成员行一致（仓库层已按 role/joined_at 排好），
// 这里再排一次就等于把排序职责藏到内存里，翻页和排序会以两套规则打架。
func TestBuildMemberItemsKeepsOrderAndSkipsMissingUser(t *testing.T) {
	joined := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	members := []model.GroupMember{
		{GroupID: 9, UserID: 1, Role: model.GroupRoleOwner, JoinedAt: joined},
		{GroupID: 9, UserID: 2, Role: model.GroupRoleMember, GroupNickname: "小二", Muted: true, JoinedAt: joined},
		{GroupID: 9, UserID: 3, Role: model.GroupRoleAdmin, JoinedAt: joined},
	}
	users := map[int64]model.PublicUser{
		1: {ID: 1, Nickname: "群主"},
		// uid=2 的资料缺失，应被跳过
		3: {ID: 3, Nickname: "管理员"},
	}

	items := buildMemberItems(members, users)
	if len(items) != 2 {
		t.Fatalf("应跳过资料缺失的成员，剩 2 个，实际 %d 个: %+v", len(items), items)
	}
	if items[0].ID != 1 || items[1].ID != 3 {
		t.Fatalf("应保持成员行原有顺序，实际 %d,%d", items[0].ID, items[1].ID)
	}
	if items[0].Role != model.GroupRoleOwner || items[1].Role != model.GroupRoleAdmin {
		t.Fatalf("角色未透传: %+v", items)
	}
	if items[0].JoinedAt != joined {
		t.Fatalf("joined_at 未透传: %v", items[0].JoinedAt)
	}
}

// TestBuildMemberItemsEmptyNonNil 验证空成员返回空切片而不是 nil。
// nil 会被 encoding/json 序列化成 null，前端对 null 做 .map 直接抛异常。
func TestBuildMemberItemsEmptyNonNil(t *testing.T) {
	items := buildMemberItems(nil, nil)
	if items == nil {
		t.Fatal("空成员应返回非 nil 切片")
	}
	if len(items) != 0 {
		t.Fatalf("空成员应返回 0 项，实际 %d", len(items))
	}
}
