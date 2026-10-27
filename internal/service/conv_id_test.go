package service

import (
	"math"
	"testing"

	"imchat/internal/model"
)

// TestPrivateConvIDSortsPair 锁住「双方算出同一个会话 ID」这个契约。
//
// 这个函数唯一的用处是让发送方和接收方落到同一条会话上。一旦漏了排序，
// A 看到的是 p_5_1、B 看到的是 p_1_5，两个人点进去就是两条各自为空的聊天
// 记录，而且两边都不会报错 —— 只有换个人再看一次才发现对不上。
func TestPrivateConvIDSortsPair(t *testing.T) {
	cases := []struct {
		a, b int64
		want string
	}{
		{1, 2, "p_1_2"},
		{2, 1, "p_1_2"},
		{2113528527860011008, 2113507465927593984, "p_2113507465927593984_2113528527860011008"},
		{2113507465927593984, 2113528527860011008, "p_2113507465927593984_2113528527860011008"},
		{0, 1, "p_0_1"},
		{-1, 5, "p_-1_5"},
		{5, -1, "p_-1_5"},
		{1, math.MaxInt64, "p_1_9223372036854775807"},
	}
	for _, c := range cases {
		if got := PrivateConvID(c.a, c.b); got != c.want {
			t.Fatalf("PrivateConvID(%d, %d) 应为 %s，实际 %s", c.a, c.b, c.want, got)
		}
	}
}

// TestPrivateTargetRoundTrip 验证「生成 → 解析」在双方视角下都还原出对方。
//
// uid 生产上是雪花 ID，恒为正；负数与 0 只作为脏输入的底线：不 panic、
// 不静默串成另一个人的 uid。贴近上界的用例是防格式化/解析两侧的位宽不一致
// （比如哪天手滑换成 FormatUint 或 32 位解析，这里会立刻炸）。
func TestPrivateTargetRoundTrip(t *testing.T) {
	cases := [][2]int64{
		{1, 2},
		{2113528527860011008, 2113507465927593984},
		{0, 1},
		{-1, 5},
		{-5, -1},
		{1, math.MaxInt64},
		{math.MaxInt64 - 1, math.MaxInt64},
	}
	for _, p := range cases {
		convID := PrivateConvID(p[0], p[1])
		if got, ok := PrivateTarget(convID, p[0]); !ok || got != p[1] {
			t.Fatalf("%s 从 uid %d 解析应得 %d，实际 %d（ok=%v）", convID, p[0], p[1], got, ok)
		}
		if got, ok := PrivateTarget(convID, p[1]); !ok || got != p[0] {
			t.Fatalf("%s 从 uid %d 解析应得 %d，实际 %d（ok=%v）", convID, p[1], p[0], got, ok)
		}
	}
}

// TestPrivateTargetRejectsOutsider 验证第三方拿别人的会话 ID 解析不出东西。
//
// 解析结果会被当成「消息发给谁」，第三人能解析成功就等于任意会话都可投递。
func TestPrivateTargetRejectsOutsider(t *testing.T) {
	convID := PrivateConvID(1, 2)
	for _, self := range []int64{0, -1, 3, math.MaxInt64} {
		if got, ok := PrivateTarget(convID, self); ok {
			t.Fatalf("uid %d 不在 %s 里，不该解析出 %d", self, convID, got)
		}
	}
}

// TestPrivateTargetRejectsMalformed 覆盖脏 ID。
//
// p_1_2_3 这类多段 ID 最危险：如果实现改成「取前两段」，它会悄悄变成一个
// 合法会话，把消息投给不相干的人；必须整串判死。
func TestPrivateTargetRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"p",
		"p_",
		"p_1",
		"p_1_2_3",
		"p_a_2",
		"p_1_b",
		"p__2",
		"p_1_2 ",
		"p_ 1_2",
		"P_1_2",
		"g_1_2",
		"x_1_2",
	}
	for _, convID := range bad {
		if got, ok := PrivateTarget(convID, 1); ok {
			t.Fatalf("%q 应被拒绝，实际解析出 %d", convID, got)
		}
	}
}

// TestGroupConvIDRoundTrip 验证群会话 ID 的生成与解析互逆。
func TestGroupConvIDRoundTrip(t *testing.T) {
	for _, gid := range []int64{0, 1, 42, math.MaxInt64} {
		convID := GroupConvID(gid)
		got, ok := GroupID(convID)
		if !ok || got != gid {
			t.Fatalf("%s 应解析回 %d，实际 %d（ok=%v）", convID, gid, got, ok)
		}
	}
	if got := GroupConvID(42); got != "g_42" {
		t.Fatalf("群会话 ID 应为 g_42，实际 %s", got)
	}
}

// TestGroupIDRejectsMalformed 覆盖非法群会话 ID，含差一位越界的数字。
//
// 越界那条是真会踩的：客户端把群 ID 当字符串透传时可能带上别的位宽，
// ParseInt 必须报错而不是截断成一个存在的 gid。
func TestGroupIDRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"g",
		"g_",
		"g_abc",
		"g_1x",
		"g_ 1",
		"G_1",
		"p_1",
		"g_9223372036854775808",
	}
	for _, convID := range bad {
		if got, ok := GroupID(convID); ok {
			t.Fatalf("%q 应被拒绝，实际解析出 %d", convID, got)
		}
	}
}

// TestParseConvDispatchesByPrefix 锁住前缀分派：g_ 走群、p_ 走私聊。
//
// 两个分支的目标 ID 都是 int64，如果 g_ 误入私聊分支，解析失败会被
// 当成「无权访问」而不是「会话不存在」，排查权限问题时会被误导。
func TestParseConvDispatchesByPrefix(t *testing.T) {
	cases := []struct {
		convID     string
		self       int64
		wantType   int8
		wantTarget int64
		wantOK     bool
	}{
		{"g_7", 1, model.ConvGroup, 7, true},
		{"p_1_2", 1, model.ConvPrivate, 2, true},
		{"p_1_2", 3, 0, 0, false},
		{"g_abc", 1, model.ConvGroup, 0, false},
		{"nope", 1, 0, 0, false},
	}
	for _, c := range cases {
		gotType, gotTarget, ok := parseConv(c.convID, c.self)
		if ok != c.wantOK || gotTarget != c.wantTarget {
			t.Fatalf("%q 由 uid %d 解析应为 (%d, ok=%v)，实际 (%d, ok=%v)",
				c.convID, c.self, c.wantTarget, c.wantOK, gotTarget, ok)
		}
		if ok && gotType != c.wantType {
			t.Fatalf("%q 的会话类型应为 %d，实际 %d", c.convID, c.wantType, gotType)
		}
	}
}
