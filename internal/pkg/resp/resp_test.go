package resp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	// Fail 的兜底分支会打 Error 日志，这里换成丢弃输出，免得测试结果里混进日志。
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	code := m.Run()

	slog.SetDefault(prev)
	os.Exit(code)
}

// newCtx 构造一个可断言状态码与响应体的 gin 上下文。
func newCtx() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// c.FullPath 在真实路由下才有值，挂个空 Request 只是因为部分 Render 路径会读它。
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, w
}

// bodyOf 解析响应体，返回业务码与原始键值，供 omitempty 一类断言使用。
func bodyOf(t *testing.T, w *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, body=%q", err, w.Body.String())
	}
	num, ok := raw["code"].(float64)
	if !ok {
		t.Fatalf("响应体缺少数字型 code: %q", w.Body.String())
	}
	return int(num), raw
}

func TestHTTPStatusOf(t *testing.T) {
	cases := []struct {
		name string
		code int
		want int
	}{
		{"参数错误", CodeInvalidParam, http.StatusBadRequest},
		{"同一前缀的兄弟码也走 400", CodeInvalidParam + 1, http.StatusBadRequest},
		{"未登录", CodeUnauthorized, http.StatusUnauthorized},
		{"令牌过期与未登录同属 401", CodeTokenExpired, http.StatusUnauthorized},
		{"无权操作", CodeForbidden, http.StatusForbidden},
		{"资源不存在", CodeNotFound, http.StatusNotFound},
		{"资源冲突", CodeConflict, http.StatusConflict},
		{"限流", CodeTooManyRequests, http.StatusTooManyRequests},
		{"内部错误", CodeInternal, http.StatusInternalServerError},
		{"5xxxx 兜底到 500", CodeInternal + 99, http.StatusInternalServerError},
		{"未登记前缀兜底到 500", 30000, http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpStatusOf(tc.code); got != tc.want {
				t.Fatalf("httpStatusOf(%d) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

func TestNewDerivesStatusFromCode(t *testing.T) {
	err := New(CodeForbidden, "只有群主能解散群")
	if err.Code != CodeForbidden {
		t.Fatalf("Code = %d, want %d", err.Code, CodeForbidden)
	}
	if err.HTTPStatus != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want %d", err.HTTPStatus, http.StatusForbidden)
	}
	// handler 与中间件大量用 err.Error() 打日志，必须等于对外文案。
	if err.Error() != "只有群主能解散群" {
		t.Fatalf("Error() = %q, want 原始 msg", err.Error())
	}
}

// TestPredefinedErrors 锁住预定义错误表与业务码表的对应关系：
// 这九条是全部接口的错误出口，改错一条就是全局文案/状态码错位。
func TestPredefinedErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        *Error
		wantCode   int
		wantStatus int
	}{
		{"参数不合法", ErrInvalidParam, CodeInvalidParam, http.StatusBadRequest},
		{"未登录", ErrUnauthorized, CodeUnauthorized, http.StatusUnauthorized},
		{"令牌过期", ErrTokenExpired, CodeTokenExpired, http.StatusUnauthorized},
		{"无权操作", ErrForbidden, CodeForbidden, http.StatusForbidden},
		{"资源不存在", ErrNotFound, CodeNotFound, http.StatusNotFound},
		{"资源冲突", ErrConflict, CodeConflict, http.StatusConflict},
		{"限流", ErrTooManyRequests, CodeTooManyRequests, http.StatusTooManyRequests},
		{"内部错误", ErrInternal, CodeInternal, http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code != tc.wantCode {
				t.Fatalf("Code = %d, want %d", tc.err.Code, tc.wantCode)
			}
			if tc.err.HTTPStatus != tc.wantStatus {
				t.Fatalf("HTTPStatus = %d, want %d", tc.err.HTTPStatus, tc.wantStatus)
			}
			if tc.err.Msg == "" {
				t.Fatal("Msg 为空会被前端展示成空白提示")
			}
		})
	}
}

func TestWithMsgKeepsCodeAndStatus(t *testing.T) {
	got := WithMsg(ErrNotFound, "用户不存在")

	if got.Code != ErrNotFound.Code {
		t.Fatalf("Code = %d, want %d", got.Code, ErrNotFound.Code)
	}
	if got.HTTPStatus != ErrNotFound.HTTPStatus {
		t.Fatalf("HTTPStatus = %d, want %d", got.HTTPStatus, ErrNotFound.HTTPStatus)
	}
	if got.Msg != "用户不存在" {
		t.Fatalf("Msg = %q, want 新文案", got.Msg)
	}
	// 文案是「换一条」而不是「改一条」：预定义错误是包级变量，
	// 一旦原地改写，后续所有请求都会拿到这条临时文案。
	if ErrNotFound.Msg == "用户不存在" {
		t.Fatal("WithMsg 污染了预定义错误，包级 ErrNotFound 被改写")
	}
	if got == ErrNotFound {
		t.Fatal("WithMsg 返回了同一个指针，调用方修改会波及全局")
	}
}

func TestFailWritesBusinessError(t *testing.T) {
	c, w := newCtx()
	Fail(c, WithMsg(ErrForbidden, "只有管理员能撤回他人消息"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("HTTP 状态 = %d, want %d", w.Code, http.StatusForbidden)
	}
	code, raw := bodyOf(t, w)
	if code != CodeForbidden {
		t.Fatalf("业务码 = %d, want %d", code, CodeForbidden)
	}
	if msg, _ := raw["msg"].(string); msg != "只有管理员能撤回他人消息" {
		t.Fatalf("msg = %q, want 自定义文案", msg)
	}
	// 错误响应不该带 data，前端按 code 分流时读到多余字段容易误判成功。
	if _, exists := raw["data"]; exists {
		t.Fatalf("错误响应不应包含 data: %q", w.Body.String())
	}
}

// TestFailUnwrapsBusinessError 覆盖 service 层用 %w 包装后再上抛的场景，
// 这是 errors.As 而不是类型断言的原因 —— 用断言会退化成 500。
func TestFailUnwrapsBusinessError(t *testing.T) {
	c, w := newCtx()
	Fail(c, fmt.Errorf("处理好友申请失败: %w", ErrNotFound))

	if w.Code != http.StatusNotFound {
		t.Fatalf("HTTP 状态 = %d, want %d", w.Code, http.StatusNotFound)
	}
	code, raw := bodyOf(t, w)
	if code != CodeNotFound {
		t.Fatalf("业务码 = %d, want %d", code, CodeNotFound)
	}
	if msg, _ := raw["msg"].(string); msg != ErrNotFound.Msg {
		t.Fatalf("msg = %q, want %q", msg, ErrNotFound.Msg)
	}
}

func TestFailHidesUnknownError(t *testing.T) {
	internal := errors.New("dial tcp 127.0.0.1:3306: connect: refused, dsn=root:p@ssw0rd@tcp/db")

	cases := []struct {
		name string
		err  error
	}{
		{"普通错误", internal},
		{"回调里传了 nil", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newCtx()
			Fail(c, tc.err)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("HTTP 状态 = %d, want %d", w.Code, http.StatusInternalServerError)
			}
			code, raw := bodyOf(t, w)
			if code != CodeInternal {
				t.Fatalf("业务码 = %d, want %d", code, CodeInternal)
			}
			msg, _ := raw["msg"].(string)
			if msg != ErrInternal.Msg {
				t.Fatalf("msg = %q, want 兜底文案 %q", msg, ErrInternal.Msg)
			}
			// 连接串、密码这类内容只能进日志，不能跟着响应体回到客户端。
			for _, leak := range []string{"3306", "dsn", "p@ssw0rd"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("兜底文案泄露了内部错误细节 %q: %q", leak, msg)
				}
			}
		})
	}
}

func TestOKOmitsEmptyData(t *testing.T) {
	t.Run("无数据时省略 data 字段", func(t *testing.T) {
		c, w := newCtx()
		OK(c, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("HTTP 状态 = %d, want 200", w.Code)
		}
		code, raw := bodyOf(t, w)
		if code != CodeOK {
			t.Fatalf("业务码 = %d, want %d", code, CodeOK)
		}
		if msg, _ := raw["msg"].(string); msg != "ok" {
			t.Fatalf("msg = %q, want ok", msg)
		}
		if _, exists := raw["data"]; exists {
			t.Fatalf("data 为 nil 时应被 omitempty 省略: %q", w.Body.String())
		}
	})

	t.Run("有数据时原样输出", func(t *testing.T) {
		c, w := newCtx()
		OK(c, gin.H{"online": true})

		_, raw := bodyOf(t, w)
		data, ok := raw["data"].(map[string]any)
		if !ok {
			t.Fatalf("data 缺失或类型不对: %q", w.Body.String())
		}
		if online, _ := data["online"].(bool); !online {
			t.Fatalf("data.online = %v, want true", data["online"])
		}
	})
}

// TestAbortStopsChain 锁住中间件语义：只写响应不 Abort 的话，
// 后续 handler 会继续跑，可能对着已写出的响应体二次写。
func TestAbortStopsChain(t *testing.T) {
	c, w := newCtx()
	Abort(c, ErrTooManyRequests)

	if !c.IsAborted() {
		t.Fatal("Abort 之后 gin 上下文应处于 aborted 状态")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("HTTP 状态 = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	code, _ := bodyOf(t, w)
	if code != CodeTooManyRequests {
		t.Fatalf("业务码 = %d, want %d", code, CodeTooManyRequests)
	}
}
