package repository

import (
	"context"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"imchat/internal/model"
)

// newDryRunDB 建一个只拼 SQL、不连库的句柄。
//
// DryRun 让 Find 跳过执行、只留 Statement，所以本机没有 MySQL 也能断言 SQL；
// DisableAutomaticPing 必须一并关掉 —— gorm.Open 默认会 ping 一次，
// 那一下就会把「离线可测」变成「离线必失败」。
func newDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "dryrun:dryrun@tcp(127.0.0.1:3306)/dryrun",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("建 dry-run 句柄失败: %v", err)
	}
	return db
}

// TestSearchByConvSQL 断言检索语句的组装结果。
//
// 重点盯 status <> 1：撤回只把状态置位、正文仍在库里，少了这个条件，
// 撤回过的内容照样能被搜出来 —— 功能上看不出任何报错，等于撤回对检索完全失效。
// 同时锁住 conv_id 必带：漏了它就变成跨会话检索，A 群的内容会出现在 B 群里。
func TestSearchByConvSQL(t *testing.T) {
	repo := &MessageRepo{db: newDryRunDB(t)}
	var list []model.Message
	stmt := repo.searchQuery(context.Background(), "g_1", "上海", 20).
		Session(&gorm.Session{DryRun: true}).Find(&list).Statement

	const wantSQL = "SELECT * FROM `messages` WHERE conv_id = ? AND content LIKE ? AND status <> ? " +
		"ORDER BY seq DESC LIMIT ?"
	if got := stmt.SQL.String(); got != wantSQL {
		t.Fatalf("SQL = %q\n期望 %q", got, wantSQL)
	}
	// 变量逐个比对：pattern 必须是两侧带通配符的形态，否则 LIKE 退化成全等匹配。
	wantVars := []any{"g_1", "%上海%", model.MsgStatusRecalled, 20}
	if len(stmt.Vars) != len(wantVars) {
		t.Fatalf("变量个数 = %d，期望 %d（%v）", len(stmt.Vars), len(wantVars), stmt.Vars)
	}
	for i := range wantVars {
		if stmt.Vars[i] != wantVars[i] {
			t.Fatalf("第 %d 个变量 = %v，期望 %v", i, stmt.Vars[i], wantVars[i])
		}
	}
}

// TestLikePattern 锁住 LIKE 通配符的转义。
//
// 这三类字符都不是理论问题：用户搜 "%" 时若不转义，pattern 变成 "%%%"，
// 命中整个会话 —— 一次本该缩小范围的操作变成全表扫描，返回的结果还与输入毫无关系，
// 用户只会认为「检索坏了」。"_" 同理会把「a_c」匹配到「abc」。
// 反斜杠必须最先转义，否则输入的 "\%" 会变成「转义符 + 通配符」而少吞一层。
func TestLikePattern(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
	}{
		{"普通词两侧加通配", "上海", "%上海%"},
		{"百分号被转义", "100%", `%100\%%`},
		{"下划线被转义", "user_id", `%user\_id%`},
		{"反斜杠被转义", `a\b`, `%a\\b%`},
		{"反斜杠在通配符之前", `\%`, `%\\\%%`},
		{"纯通配符不再扩大范围", "%_", `%\%\_%`},
		{"空串只得到一对通配符", "", "%%"},
		{"连字符与数字无需转义", "2026-11", "%2026-11%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := likePattern(c.keyword); got != c.want {
				t.Fatalf("likePattern(%q) = %q，期望 %q", c.keyword, got, c.want)
			}
		})
	}
}
