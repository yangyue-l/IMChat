package service

import (
	"testing"

	"imchat/internal/media"
	"imchat/internal/model"
)

const testCDN = "https://cdn.example.com"

// initMedia 给 media 装上测试用前缀，用例结束后复位，避免影响同包其它用例。
func initMedia(t *testing.T) {
	t.Helper()
	media.Init(testCDN)
	t.Cleanup(func() { media.Init("") })
}

// TestStripExtraURL 锁住「extra 里与 content 重复的图片地址」这条清理规则。
//
// 只删完全相同的那一份：客户端如果放的是缩略图之类的别的值，服务端不该替它
// 做决定；extra 不是 JSON 时更不能动，否则客户端自定义字段会被抹掉。
func TestStripExtraURL(t *testing.T) {
	url := testCDN + "/u/1/2026/11/18/a.png"
	cases := []struct {
		name, extra, content, want string
	}{
		{"与 content 相同则删除", `{"url":"` + url + `"}`, url, ""},
		{"其它字段保留", `{"url":"` + url + `","quote":{"msg_id":"1"}}`, url, `{"quote":{"msg_id":"1"}}`},
		{"url 不同则原样保留", `{"url":"` + testCDN + `/thumb.png"}`, url, `{"url":"` + testCDN + `/thumb.png"}`},
		{"没有 url 字段原样保留", `{"quote":{"msg_id":"1"}}`, url, `{"quote":{"msg_id":"1"}}`},
		{"不是 JSON 原样保留", "not-json", url, "not-json"},
		{"extra 为空", "", url, ""},
		{"content 为空", `{"url":"x"}`, "", `{"url":"x"}`},
		// 大整数不能因为过一次 map[string]any 就丢精度
		{"自定义数字字段保持原样", `{"url":"` + url + `","n":12345678901234567890}`, url, `{"n":12345678901234567890}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripExtraURL(c.extra, c.content); got != c.want {
				t.Fatalf("stripExtraURL(%s) = %s，期望 %s", c.extra, got, c.want)
			}
		})
	}
}

// TestToDTOResolvesImageContent 是「库里存 key、出站拼地址」的落点。
//
// 文本消息必须原样返回：把普通文本也过一遍 media.URL，会把「hi」拼成
// https://cdn.example.com/hi，这类错误在单测里不锁住就只能等线报。
func TestToDTOResolvesImageContent(t *testing.T) {
	initMedia(t)

	img := &model.Message{MsgType: model.MsgImage, Content: "u/1/2026/11/18/a.png"}
	if got, want := toDTO(img, nil).Content, testCDN+"/u/1/2026/11/18/a.png"; got != want {
		t.Fatalf("图片消息 content = %q，期望 %q", got, want)
	}

	// 存量数据：改造前入库的是绝对地址，出站必须原样返回
	legacy := &model.Message{MsgType: model.MsgImage, Content: "https://old.example.com/a.png"}
	if got := toDTO(legacy, nil).Content; got != "https://old.example.com/a.png" {
		t.Fatalf("存量图片地址被改写: %q", got)
	}

	txt := &model.Message{MsgType: model.MsgText, Content: "hi"}
	if got := toDTO(txt, nil).Content; got != "hi" {
		t.Fatalf("文本消息 content = %q，期望原样返回", got)
	}
}
