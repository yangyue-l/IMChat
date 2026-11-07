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

// queryNonNegInt 解析非负整数查询参数，缺省返回 0；非整数或负数报参数错误。
// 为什么不像以前那样「解析失败就用缺省值」，见 queryPaging 的说明。
func queryNonNegInt(c *gin.Context, name string) (int, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, resp.WithMsg(resp.ErrInvalidParam, "参数 "+name+" 非法")
	}
	return v, nil
}

// queryNonNegInt64 同 queryNonNegInt，用于 Snowflake 量级的游标。
//
// 不复用 queryNonNegInt：游标是 int64（seq、动态 ID），走 Atoi 会受 int 宽度限制，
// 在 32 位平台上会把合法游标判成非法。
func queryNonNegInt64(c *gin.Context, name string) (int64, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, resp.WithMsg(resp.ErrInvalidParam, "参数 "+name+" 非法")
	}
	return v, nil
}

// queryPaging 解析 offset/limit 式分页，缺省返回 0 交由 service 归一。
//
// 这里刻意不用「解析失败就用 fallback」的写法：offset 静默归零会让无限滚动
// 永远停在第一页、反复拉同一批数据，而客户端完全看不出自己传错了；
// limit 静默归缺省值则会让客户端以为拿到了它要的条数。分页参数错了就报错。
func queryPaging(c *gin.Context) (offset, limit int, err error) {
	if offset, err = queryNonNegInt(c, "offset"); err != nil {
		return 0, 0, err
	}
	if limit, err = queryNonNegInt(c, "limit"); err != nil {
		return 0, 0, err
	}
	return offset, limit, nil
}

// queryCursorPage 解析「游标 + limit」式分页，缺省返回 0 交由 service 归一。
//
// 与 queryPaging 同一套严格口径。两种形状分开而不是合成一个函数，是因为游标
// 参数名（before_seq / before_id）由各接口自己定，塞进一个函数只能靠猜。
func queryCursorPage(c *gin.Context, cursorName string) (cursor int64, limit int, err error) {
	if cursor, err = queryNonNegInt64(c, cursorName); err != nil {
		return 0, 0, err
	}
	if limit, err = queryNonNegInt(c, "limit"); err != nil {
		return 0, 0, err
	}
	return cursor, limit, nil
}
