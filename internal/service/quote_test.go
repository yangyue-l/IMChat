package service

import (
	"encoding/json"
	"strings"
	"testing"

	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// snowflakeID 取一个真实的雪花 ID：它会超过 float64 的精确整数范围（2^53），
// 用来锁住「引用 ID 必须以字符串形式进出 JSON」这条约定。
const snowflakeID int64 = 2113507230744580097

// TestCheckQuote 覆盖引用的三条拒绝分支。
//
// 三条都不是「顺手校验」：跨会话引用会把 A 会话的内容送进 B 会话，属于越权泄露；
// 引用已撤回的消息，等于给一条用户已经收回的内容留了个永久入口。
func TestCheckQuote(t *testing.T) {
	normal := &model.Message{BaseModel: model.BaseModel{ID: 8}, ConvID: "p_1_2"}
	recalled := &model.Message{
		BaseModel: model.BaseModel{ID: 9}, ConvID: "p_1_2", Status: model.MsgStatusRecalled,
	}
	cases := []struct {
		name     string
		msg      *model.Message
		convID   string
		wantCode int
	}{
		{"消息不存在", nil, "p_1_2", resp.CodeInvalidParam},
		{"零 ID 视为不存在", &model.Message{}, "p_1_2", resp.CodeInvalidParam},
		{"跨会话引用被拒", normal, "p_1_3", resp.CodeInvalidParam},
		{"群聊引用私聊消息被拒", normal, "g_9", resp.CodeInvalidParam},
		{"引用已撤回消息被拒", recalled, "p_1_2", resp.CodeInvalidParam},
		{"同会话正常消息放行", normal, "p_1_2", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkQuote(c.msg, c.convID)
			if c.wantCode == 0 {
				if err != nil {
					t.Fatalf("应放行，实际 %v", err)
				}
				return
			}
			if got := errCode(err); got != c.wantCode {
				t.Fatalf("errCode = %d，期望 %d（err=%v）", got, c.wantCode, err)
			}
		})
	}
}

// TestQuotePreview 锁住摘要口径：占位文案、rune 截断、长度边界。
// 按 rune 而不是字节截断，否则中文会被切成半个字，前端渲染出替换符。
func TestQuotePreview(t *testing.T) {
	cases := []struct {
		name string
		msg  *model.Message
		want string
	}{
		{"图片用占位文案", &model.Message{MsgType: model.MsgImage, Content: "whatever"}, "[图片]"},
		{"文件用占位文案", &model.Message{MsgType: model.MsgFile, Content: "a.zip"}, "[文件]"},
		{"短文本原样返回", &model.Message{MsgType: model.MsgText, Content: "在吗"}, "在吗"},
		{
			"恰好 64 字不截断",
			&model.Message{MsgType: model.MsgText, Content: strings.Repeat("引", maxQuotePreviewRunes)},
			strings.Repeat("引", maxQuotePreviewRunes),
		},
		{
			"超出 64 字截断并加省略号",
			&model.Message{MsgType: model.MsgText, Content: strings.Repeat("引", maxQuotePreviewRunes+1)},
			strings.Repeat("引", maxQuotePreviewRunes) + "...",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quotePreview(c.msg); got != c.want {
				t.Fatalf("quotePreview = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestMergeQuoteExtra 覆盖合并规则：保留客户端自带字段、坏 extra 报参数错、
// 结果超列宽报参数错。最后一条尤其重要 —— 让它直接落库就是 MySQL 1406，前端只看得到 500。
func TestMergeQuoteExtra(t *testing.T) {
	quote := &QuoteDTO{MsgID: snowflakeID, FromID: 2113507230744580098, Preview: "在吗"}

	t.Run("空 extra 只产出 quote", func(t *testing.T) {
		out, err := mergeQuoteExtra("", quote)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &fields); err != nil {
			t.Fatalf("结果不是 JSON 对象: %v（%s）", err, out)
		}
		if len(fields) != 1 {
			t.Fatalf("应只有 quote 一个字段，实际 %v", out)
		}
		// 大整数必须序列化成字符串，否则前端 JSON.parse 会丢精度
		if !strings.Contains(string(fields["quote"]), `"msg_id":"2113507230744580097"`) {
			t.Fatalf("msg_id 应序列化为字符串，实际 %s", fields["quote"])
		}
	})

	t.Run("保留客户端已有字段", func(t *testing.T) {
		out, err := mergeQuoteExtra(`{"width":800,"height":600}`, quote)
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &fields); err != nil {
			t.Fatalf("结果不是 JSON 对象: %v", err)
		}
		if string(fields["width"]) != "800" || string(fields["height"]) != "600" {
			t.Fatalf("客户端的 width/height 被改动或丢失: %s", out)
		}
		if _, ok := fields["quote"]; !ok {
			t.Fatalf("缺 quote 字段: %s", out)
		}
	})

	t.Run("null 不 panic 且能合并", func(t *testing.T) {
		if _, err := mergeQuoteExtra("null", quote); err != nil {
			t.Fatalf("extra 为 null 应可合并，实际 %v", err)
		}
	})

	t.Run("非对象 JSON 被拒", func(t *testing.T) {
		for _, bad := range []string{`"str"`, `[1,2]`, `123`, `", "`} {
			if got := errCode(mustFail(t, bad, quote)); got != resp.CodeInvalidParam {
				t.Fatalf("extra=%s 应返回参数错误，实际 %d", bad, got)
			}
		}
	})

	t.Run("合并后超 1024 字节被拒", func(t *testing.T) {
		base := `{"blob":"` + strings.Repeat("a", 1000) + `"}`
		if len(base) > maxExtraBytes {
			t.Fatalf("用例前提不成立：base 本身 %d 字节已超列宽", len(base))
		}
		if got := errCode(mustFail(t, base, quote)); got != resp.CodeInvalidParam {
			t.Fatalf("超列宽应返回参数错误，实际 %d", got)
		}
	})
}

// mustFail 断言 mergeQuoteExtra 失败并返回错误。
func mustFail(t *testing.T, extra string, quote *QuoteDTO) error {
	t.Helper()
	out, err := mergeQuoteExtra(extra, quote)
	if err == nil {
		t.Fatalf("extra=%s 应报错，实际返回 %s", extra, out)
	}
	return err
}

// TestQuoteOf 验证读取侧：与写入侧往返一致、老消息（没有 quote）返回 nil。
// 读取侧不能因为 extra 是脏数据就 panic 或返回空结构体 —— 历史消息里还有
// 引用功能上线前写入的 {"width":800} 这类内容。
func TestQuoteOf(t *testing.T) {
	merged, err := mergeQuoteExtra(`{"width":800}`, &QuoteDTO{
		MsgID: snowflakeID, FromID: 7, Preview: "在吗",
	})
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	t.Run("往返一致", func(t *testing.T) {
		got := quoteOf(merged)
		if got == nil {
			t.Fatalf("应解析出 quote: %s", merged)
		}
		if got.MsgID != snowflakeID || got.FromID != 7 || got.Preview != "在吗" {
			t.Fatalf("字段不一致: %+v", got)
		}
	})

	cases := []struct {
		name  string
		extra string
	}{
		{"空 extra", ""},
		{"没有 quote 的老消息", `{"width":800}`},
		{"不是 JSON", "width=800"},
		{"quote 为 null", `{"quote":null}`},
		{"msg_id 缺失", `{"quote":{"from_id":"7"}}`},
		{"msg_id 为负", `{"quote":{"msg_id":"-1"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quoteOf(c.extra); got != nil {
				t.Fatalf("应返回 nil，实际 %+v", got)
			}
		})
	}
}

// TestToDTOExposesQuote 锁住对外结构：引用信息必须出现在 DTO.Quote 上，
// 而不是只躺在 extra 字符串里让客户端自己解析两遍。
func TestToDTOExposesQuote(t *testing.T) {
	extra, err := mergeQuoteExtra("", &QuoteDTO{MsgID: snowflakeID, FromID: 7, Preview: "在吗"})
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	dto := toDTO(&model.Message{
		BaseModel: model.BaseModel{ID: snowflakeID}, ConvID: "p_1_2",
		MsgType: model.MsgText, Content: "好的", Extra: extra,
	}, nil)
	if dto.Quote == nil || dto.Quote.MsgID != snowflakeID {
		t.Fatalf("DTO 未带出引用信息: %+v", dto)
	}
	// Extra 保持原样下发，兼容仍在读 extra 的旧客户端
	if dto.Extra != extra {
		t.Fatalf("Extra 被改写: %q", dto.Extra)
	}
}
