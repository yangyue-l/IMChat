// Package resp 统一定义 HTTP 响应结构、业务错误码与错误转换，避免各 handler 各写一套。
package resp

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 业务码约定：0 成功；4xxxx 客户端错误；5xxxx 服务端错误。
const (
	CodeOK              = 0
	CodeInvalidParam    = 40000
	CodeUnauthorized    = 40100
	CodeTokenExpired    = 40101
	CodeForbidden       = 40300
	CodeNotFound        = 40400
	CodeConflict        = 40900
	CodeTooManyRequests = 42900
	CodeInternal        = 50000
)

// Body 是所有接口的统一响应体。
type Body struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// Error 是可携带业务码与 HTTP 状态的应用错误。
type Error struct {
	Code       int
	Msg        string
	HTTPStatus int
}

func (e *Error) Error() string { return e.Msg }

// New 构造应用错误，HTTP 状态由业务码前三位推导。
func New(code int, msg string) *Error {
	return &Error{Code: code, Msg: msg, HTTPStatus: httpStatusOf(code)}
}

func httpStatusOf(code int) int {
	switch code / 100 {
	case 400:
		return http.StatusBadRequest
	case 401:
		return http.StatusUnauthorized
	case 403:
		return http.StatusForbidden
	case 404:
		return http.StatusNotFound
	case 409:
		return http.StatusConflict
	case 429:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// 预定义错误，业务层直接返回，避免散落魔法字符串。
var (
	ErrInvalidParam    = New(CodeInvalidParam, "参数不合法")
	ErrUnauthorized    = New(CodeUnauthorized, "未登录或登录已失效")
	ErrTokenExpired    = New(CodeTokenExpired, "登录已过期，请重新登录")
	ErrForbidden       = New(CodeForbidden, "无权操作")
	ErrNotFound        = New(CodeNotFound, "资源不存在")
	ErrConflict        = New(CodeConflict, "资源冲突")
	ErrTooManyRequests = New(CodeTooManyRequests, "请求过于频繁，请稍后再试")
	ErrInternal        = New(CodeInternal, "服务器开小差了")
)

// WithMsg 基于预定义错误换一条更具体的提示，保留原业务码。
func WithMsg(base *Error, msg string) *Error {
	return &Error{Code: base.Code, Msg: msg, HTTPStatus: base.HTTPStatus}
}

// OK 输出成功响应。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Body{Code: CodeOK, Msg: "ok", Data: data})
}

// Fail 输出错误响应；未知错误统一兜底为 50000，并记录日志。
func Fail(c *gin.Context, err error) {
	var ae *Error
	if errors.As(err, &ae) {
		c.JSON(ae.HTTPStatus, Body{Code: ae.Code, Msg: ae.Msg})
		return
	}
	slog.Error("接口未处理错误", "path", c.FullPath(), "err", err)
	c.JSON(http.StatusInternalServerError, Body{Code: CodeInternal, Msg: ErrInternal.Msg})
}

// Abort 输出错误响应并中断中间件链，供中间件使用。
func Abort(c *gin.Context, err error) {
	Fail(c, err)
	c.Abort()
}
