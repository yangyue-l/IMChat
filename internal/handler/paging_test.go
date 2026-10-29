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
				if err == nil {
					t.Fatalf("期望参数错误，实际返回 offset=%d limit=%d", off, lim)
				}
				var ae *resp.Error
				if !errors.As(err, &ae) || ae.Code != resp.CodeInvalidParam {
					t.Fatalf("期望 40000 参数错误，实际 %v", err)
				}
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
