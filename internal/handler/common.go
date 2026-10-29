package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"imchat/internal/pkg/resp"
)

// bindJSON 解析请求体，失败时返回统一参数错误。
func bindJSON(c *gin.Context, dst any) error {
	if err := c.ShouldBindJSON(dst); err != nil {
		return resp.WithMsg(resp.ErrInvalidParam, "请求参数解析失败: "+err.Error())
	}
	return nil
}

// pathID 解析路径中的 int64 参数。
func pathID(c *gin.Context, name string) (int64, error) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, resp.WithMsg(resp.ErrInvalidParam, "参数 "+name+" 非法")
	}
	return id, nil
}

// queryInt64 解析查询参数中的 int64，缺省返回 fallback。
func queryInt64(c *gin.Context, name string, fallback int64) int64 {
	raw := c.Query(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

// queryInt 解析查询参数中的 int，缺省返回 fallback。
func queryInt(c *gin.Context, name string, fallback int) int {
	raw := c.Query(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// queryPaging 解析分页参数 offset/limit，缺省返回 0 交由 service 归一。
//
// 这里刻意不学 queryInt 的「解析失败就用 fallback」：offset 静默归零会让无限滚动
// 永远停在第一页、反复拉同一批数据，而客户端完全看不出自己传错了；
// limit 静默归缺省值则会让客户端以为拿到了它要的条数。分页参数错了就报错。
func queryPaging(c *gin.Context) (offset, limit int, err error) {
	for _, f := range []struct {
		name string
		dst  *int
	}{{"offset", &offset}, {"limit", &limit}} {
		raw := c.Query(f.name)
		if raw == "" {
			continue
		}
		v, convErr := strconv.Atoi(raw)
		if convErr != nil || v < 0 {
			return 0, 0, resp.WithMsg(resp.ErrInvalidParam, "参数 "+f.name+" 非法")
		}
		*f.dst = v
	}
	return offset, limit, nil
}
