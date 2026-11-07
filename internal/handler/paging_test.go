package handler

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"imchat/internal/pkg/resp"
)

// newQueryCtx 构造一个只带查询串的 gin 上下文，用于测纯解析函数。
func newQueryCtx(target string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", target, nil)
	return c
}

// requireParamErr 断言错误是 40000 参数错误。
func requireParamErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("期望参数错误，实际没有报错")
	}
	var ae *resp.Error
	if !errors.As(err, &ae) || ae.Code != resp.CodeInvalidParam {
		t.Fatalf("期望 40000 参数错误，实际 %v", err)
	}
}

// TestQueryPagingParses 验证缺省与显式取值。
func TestQueryPagingParses(t *testing.T) {
	cases := []struct {
		name           string
		target         string
		wantOff, wantL int
	}{
		{"都缺省交给 service 归一", "/groups/1/members", 0, 0},
		{"显式传值", "/groups/1/members?offset=40&limit=20", 40, 20},
		{"只传 offset", "/groups/1/members?offset=7", 7, 0},
		{"零值合法", "/groups/1/members?offset=0&limit=0", 0, 0},
		{"非整数报错", "/groups/1/members?offset=abc", -1, -1},
		{"负数报错", "/groups/1/members?limit=-1", -1, -1},
		{"limit 非整数报错", "/groups/1/members?limit=1.5", -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off, lim, err := queryPaging(newQueryCtx(c.target))
			if c.wantOff == -1 {
				requireParamErr(t, err)
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if off != c.wantOff || lim != c.wantL {
				t.Fatalf("得到 (%d,%d)，期望 (%d,%d)", off, lim, c.wantOff, c.wantL)
			}
		})
	}
}

// TestQueryCursorPageParses 验证游标分页的缺省、显式取值与非法值分流。
//
// 「缺省一律给 0」是刻意保留的：缺省值（消息 20 条、动态 10 条）各自住在 service 里，
// handler 再抄一份就会漂移 —— 用例断言 0，就是锁住「handler 不复制默认值」这条约定。
func TestQueryCursorPageParses(t *testing.T) {
	cases := []struct {
		name         string
		cursorName   string
		target       string
		wantCursor   int64
		wantLimit    int
		wantParamErr bool
	}{
		{"都缺省", "before_seq", "/messages?conv_id=p_1_2", 0, 0, false},
		{"显式传值", "before_seq", "/messages?conv_id=p_1_2&before_seq=120&limit=20", 120, 20, false},
		{"limit 为 0 视为没传", "before_seq", "/messages?conv_id=p_1_2&before_seq=120&limit=0", 120, 0, false},
		{"游标为 0 视为没传", "before_seq", "/messages?conv_id=p_1_2&before_seq=0&limit=20", 0, 20, false},
		{"游标非整数报错", "before_seq", "/messages?conv_id=p_1_2&before_seq=abc", 0, 0, true},
		{"游标负数报错", "before_seq", "/messages?conv_id=p_1_2&before_seq=-1", 0, 0, true},
		{"limit 非整数报错", "before_seq", "/messages?conv_id=p_1_2&limit=十", 0, 0, true},
		{"limit 负数报错", "before_seq", "/messages?conv_id=p_1_2&limit=-5", 0, 0, true},
		// 游标名由调用方指定：动态用 before_id，不能硬编码成 before_seq
		{"另一套游标名", "before_id", "/moments/timeline?before_id=99&limit=10", 99, 10, false},
		// Snowflake ID 超过 int32，走 Atoi 的实现在 32 位平台上会判成非法
		{"Snowflake 量级游标", "before_id", "/moments/timeline?before_id=2113507230744580097", 2113507230744580097, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cursor, limit, err := queryCursorPage(newQueryCtx(c.target), c.cursorName)
			if c.wantParamErr {
				requireParamErr(t, err)
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if cursor != c.wantCursor || limit != c.wantLimit {
				t.Fatalf("得到 (%d,%d)，期望 (%d,%d)", cursor, limit, c.wantCursor, c.wantLimit)
			}
		})
	}
}

// TestQueryNonNegIntParses 覆盖搜索这类「只有 limit」的接口。
func TestQueryNonNegIntParses(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		want         int
		wantParamErr bool
	}{
		{"缺省返回 0", "/users/search?keyword=ali", 0, false},
		{"显式传值", "/users/search?keyword=ali&limit=20", 20, false},
		{"上限不在 handler 判定", "/users/search?keyword=ali&limit=5000", 5000, false},
		{"非整数报错", "/users/search?keyword=ali&limit=abc", 0, true},
		{"负数报错", "/users/search?keyword=ali&limit=-1", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := queryNonNegInt(newQueryCtx(c.target), "limit")
			if c.wantParamErr {
				requireParamErr(t, err)
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("得到 %d，期望 %d", got, c.want)
			}
		})
	}
}
