package model

import (
	"testing"

	"imchat/internal/media"
)

// TestToPublicResolvesAvatar 锁住用户头像的出站口径。
//
// ToPublic 是所有用户投影（好友列表、群成员、消息发送者、点赞/评论者）的
// 唯一转换点，这里把 key 拼成绝对地址，其它接口就不会返回裸 key。
// 存量数据是绝对地址，必须原样返回 —— 拼上前缀会得到一条打不开的地址。
func TestToPublicResolvesAvatar(t *testing.T) {
	media.Init("https://cdn.example.com")
	t.Cleanup(func() { media.Init("") })

	cases := []struct {
		name, in, want string
	}{
		{"key 拼成绝对地址", "u/1/2026/11/18/a.png", "https://cdn.example.com/u/1/2026/11/18/a.png"},
		{"存量绝对地址原样返回", "https://old.example.com/a.png", "https://old.example.com/a.png"},
		{"默认头像占位符原样返回", "avatar://A", "avatar://A"},
		{"空头像", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &User{Avatar: c.in}
			if got := u.ToPublic().Avatar; got != c.want {
				t.Fatalf("ToPublic().Avatar = %q，期望 %q", got, c.want)
			}
		})
	}
}

// 群头像走 AvatarURL，与用户头像是同一套规则。
func TestGroupAvatarURL(t *testing.T) {
	media.Init("https://cdn.example.com")
	t.Cleanup(func() { media.Init("") })

	if got := (&Group{Avatar: "u/2/2026/11/18/g.png"}).AvatarURL(); got != "https://cdn.example.com/u/2/2026/11/18/g.png" {
		t.Fatalf("AvatarURL = %q", got)
	}
	if got := (&Group{Avatar: ""}).AvatarURL(); got != "" {
		t.Fatalf("空群头像应返回空串，实际 %q", got)
	}
}
