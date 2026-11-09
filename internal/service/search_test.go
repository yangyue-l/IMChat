package service

import (
	"context"
	"strings"
	"testing"

	"imchat/internal/pkg/resp"
)

// TestNormalizeKeyword 锁住检索关键词的边界与裁剪口径。
//
// 长度按 rune 而不是字节计：一个汉字占 3 字节，按字节卡 32 只剩 10 个汉字，
// 中文用户随手一句关键词就被判超长，接口等于只对英文可用。
// 用例里 11 个汉字（33 字节）必须放行，这一条专门盯住「字节计数」的回归。
func TestNormalizeKeyword(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		wantCode int
	}{
		{"空串被拒", "", "", resp.CodeInvalidParam},
		{"纯空白被拒", "   \t\n ", "", resp.CodeInvalidParam},
		{"单字太短", "上", "", resp.CodeInvalidParam},
		{"首尾空白裁剪后只剩单字", "  上  ", "", resp.CodeInvalidParam},
		{"两字边界放行", "上海", "上海", 0},
		{"裁剪首尾空白", "  上海  ", "上海", 0},
		{"11 个汉字按 rune 计数放行（33 字节）", strings.Repeat("中", 11), strings.Repeat("中", 11), 0},
		{"32 个汉字到达上限", strings.Repeat("喵", maxKeywordRunes), strings.Repeat("喵", maxKeywordRunes), 0},
		{"33 个汉字超限", strings.Repeat("喵", maxKeywordRunes+1), "", resp.CodeInvalidParam},
		{"ASCII 32 位放行", strings.Repeat("a", maxKeywordRunes), strings.Repeat("a", maxKeywordRunes), 0},
		{"ASCII 33 位超限", strings.Repeat("a", maxKeywordRunes+1), "", resp.CodeInvalidParam},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeKeyword(c.in)
			if c.wantCode != 0 {
				if code := errCode(err); code != c.wantCode {
					t.Fatalf("errCode = %d，期望 %d（err=%v）", code, c.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应放行，实际 %v", err)
			}
			if got != c.want {
				t.Fatalf("normalizeKeyword = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestNormalizeSearchLimit 锁住检索条数的归一化口径：缺省 20、超限压到 50。
//
// 超限必须压到上限而不是退回默认值：退回默认值会让 limit=51 只拿到 20 条，
// 比 limit=50 的 50 条还少，客户端按「越大越多」的理解直接拿到反直觉的结果。
func TestNormalizeSearchLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultSearchLimit},
		{-1, defaultSearchLimit},
		{1, 1},
		{maxSearchLimit, maxSearchLimit},
		{maxSearchLimit + 1, maxSearchLimit},
		{1000, maxSearchLimit},
	}
	for _, c := range cases {
		if got := normalizeSearchLimit(c.in); got != c.want {
			t.Fatalf("normalizeSearchLimit(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestSearchChecksBeforeTouchingDeps 锁住 Search 的两道前置校验，以及它们的先后。
//
// 这里刻意用 deps / rel 全为 nil 的 MessageService 调用：只要有一处提前摸到
// 依赖就会 panic，所以「跑得通」本身就证明了校验确实在查库之前返回。
//
// 顺序是「参数 → 鉴权」而不是反过来：参数校验不查库、也不泄露会话是否存在，
// 放在前面能让非法请求省掉一次 DB 往返；鉴权仍然早于消息查询，越权拿不到任何数据。
func TestSearchChecksBeforeTouchingDeps(t *testing.T) {
	svc := &MessageService{}
	cases := []struct {
		name     string
		convID   string
		keyword  string
		wantCode int
	}{
		{"关键词太短先于鉴权", "g_abc", "上", resp.CodeInvalidParam},
		{"空关键词先于鉴权", "", "", resp.CodeInvalidParam},
		{"关键词超长先于鉴权", "g_abc", strings.Repeat("喵", maxKeywordRunes+1), resp.CodeInvalidParam},
		{"空 conv_id", "", "上海", resp.CodeForbidden},
		{"群会话 ID 非法", "g_abc", "上海", resp.CodeForbidden},
		{"不是我参与的私聊会话", "p_8_9", "上海", resp.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list, err := svc.Search(context.Background(), 1, c.convID, c.keyword, 20)
			if code := errCode(err); code != c.wantCode {
				t.Fatalf("errCode = %d，期望 %d（err=%v）", code, c.wantCode, err)
			}
			if list != nil {
				t.Fatalf("出错时不应返回结果，实际 %v", list)
			}
		})
	}
}
