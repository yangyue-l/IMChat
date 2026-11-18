package storage

import (
	"context"
	"strings"
	"testing"
)

// validConfig 是一份字段齐全的配置。
// 构造本身不发网络请求，所以这些用例不需要 OSS 凭据。
func validConfig() Config {
	return Config{
		Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "imchat-test",
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"缺 endpoint", func(c *Config) { c.Endpoint = "" }, "endpoint"},
		{"缺 bucket", func(c *Config) { c.Bucket = "" }, "bucket"},
		{"缺 AK", func(c *Config) { c.AccessKeyID = "" }, "凭据"},
		{"缺 SK", func(c *Config) { c.AccessKeySecret = "" }, "凭据"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mod(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatalf("%s 时应当报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案未点明原因: %v", err)
			}
		})
	}
}

// 请求已取消时不能再去打网络：这里只验证早退，不会真的发出请求。
func TestPutCanceledContext(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	url, err := st.Put(ctx, Object{Key: "u/1/2026/11/18/1.png", Body: strings.NewReader("x"), Size: 1})
	if err == nil {
		t.Fatal("已取消的上下文应当报错")
	}
	if url != "" {
		t.Fatalf("失败时不应返回 url，实际 %q", url)
	}
}

// 空列表不该发出请求，否则每次「没有旧图可删」的操作都会多一次 OSS 往返。
func TestDeleteWithoutKeys(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.Delete(context.Background()); err != nil {
		t.Fatalf("空列表应当直接返回 nil，实际 %v", err)
	}
}

// 删除同样要先看 context：请求已取消时不必再打一次网络。
func TestDeleteCanceledContext(t *testing.T) {
	st, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := st.Delete(ctx, "u/1/2026/11/18/1.png"); err == nil {
		t.Fatal("已取消的上下文应当报错")
	}
}
