package media

import (
	"os"
	"strings"
	"testing"
	"time"

	"imchat/internal/pkg/snowflake"
)

func TestMain(m *testing.M) {
	// key 生成依赖雪花 ID，未初始化会 panic
	if err := snowflake.Init(1); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

const cdn = "https://cdn.example.com"

// initCDN 每个用例自己决定前缀，避免用例之间靠执行顺序传递状态。
func initCDN(t *testing.T, base string) {
	t.Helper()
	Init(base)
	t.Cleanup(func() { Init("") })
}

func TestURL(t *testing.T) {
	initCDN(t, cdn+"/")

	cases := []struct {
		name, in, want string
	}{
		// 尾斜杠与前导斜杠都不能拼出双斜杠
		{"key 拼前缀", "u/1/2026/11/18/1.png", cdn + "/u/1/2026/11/18/1.png"},
		{"前导斜杠归一化", "/u/1/2026/11/18/1.png", cdn + "/u/1/2026/11/18/1.png"},
		// 历史数据与用户自填的外部图必须原样返回，拼前缀就整条地址报废
		{"存量绝对地址原样返回", "https://old.example.com/a.png", "https://old.example.com/a.png"},
		{"占位符原样返回", "avatar://A", "avatar://A"},
		{"空值原样返回", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := URL(c.in); got != c.want {
				t.Fatalf("URL(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// 前缀没配置时不能拼出 "/key" 这种只有一半的地址：宁可原样返回，
// 让前端按同源相对路径处理，也不要造一个看起来像绝对地址的坏值。
func TestURLWithoutBase(t *testing.T) {
	initCDN(t, "")
	if got := URL("u/1/1.png"); got != "u/1/1.png" {
		t.Fatalf("URL = %q，期望原样返回", got)
	}
}

func TestURLs(t *testing.T) {
	initCDN(t, cdn)

	// nil 与空切片要能区分：动态没有配图时前端拿到 null，不是 []
	if got := URLs(nil); got != nil {
		t.Fatalf("nil 应当返回 nil，实际 %#v", got)
	}
	got := URLs([]string{"u/1/a.png", "avatar://A"})
	want := []string{cdn + "/u/1/a.png", "avatar://A"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("URLs = %v，期望 %v", got, want)
	}
}

// Key 是 URL 的逆运算：前端把上传接口返回的地址原样回传，写入前必须还原成 key，
// 否则库里存的还是绝对地址，换 CDN 依然要回刷数据。
func TestKeyRoundTrip(t *testing.T) {
	initCDN(t, cdn)

	cases := []struct {
		name, in, want string
	}{
		{"绝对地址还原成 key", cdn + "/u/7/2026/11/18/1.png", "u/7/2026/11/18/1.png"},
		{"key 原样保留", "u/7/2026/11/18/1.png", "u/7/2026/11/18/1.png"},
		{"前导斜杠归一化", "/u/7/2026/11/18/1.png", "u/7/2026/11/18/1.png"},
		// 别人的域名不是本服务发的，不能当成 key 存进去
		{"外部地址原样保留", "https://img.other.com/a.png", "https://img.other.com/a.png"},
		{"占位符原样保留", "avatar://A", "avatar://A"},
		{"空值原样保留", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Key(c.in); got != c.want {
				t.Fatalf("Key(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}

	key := NewKey(time.Now(), 7, ".webp")
	if got := Key(URL(key)); got != key {
		t.Fatalf("URL→Key 未回到原值：%q → %q", key, got)
	}
}

func TestNewKey(t *testing.T) {
	now := time.Date(2026, 11, 18, 21, 0, 0, 0, time.Local)
	key := NewKey(now, 1001, ".webp")

	// 对象存储的 key 只能用 '/'，出现反斜杠会把整条路径变成一个扁平对象名
	if strings.Contains(key, `\`) {
		t.Fatalf("key 含有反斜杠: %q", key)
	}
	// 上传者 uid 必须在第一级目录：回收前靠它判断对象归属，见 Owned
	if !strings.HasPrefix(key, "u/1001/2026/11/18/") {
		t.Fatalf("key 未按 上传者/日期 分目录: %q", key)
	}
	if !strings.HasSuffix(key, ".webp") {
		t.Fatalf("key 丢了扩展名: %q", key)
	}
	if NewKey(now, 1001, ".webp") == key {
		t.Fatal("同一时刻生成的 key 不应重复")
	}
}

func TestOwner(t *testing.T) {
	initCDN(t, cdn)

	cases := []struct {
		name  string
		in    string
		uid   int64
		owned bool
	}{
		{"自己传的", "u/1001/2026/11/18/1.png", 1001, true},
		{"绝对地址也能解析", cdn + "/u/1001/2026/11/18/1.png", 1001, true},
		{"别人传的", "u/1001/2026/11/18/1.png", 1002, false},
		// 历史数据（改造前按日期直接分目录）没有 uid 段，不能猜归属
		{"历史 key", "2026/11/18/1.png", 1001, false},
		{"占位符", "avatar://A", 1001, false},
		{"外部地址", "https://img.other.com/u/1001/a.png", 1001, false},
		{"uid 非数字", "u/abc/2026/11/18/1.png", 1001, false},
		{"uid 为零", "u/0/2026/11/18/1.png", 0, false},
		{"空值", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Owned(c.in, c.uid); got != c.owned {
				t.Fatalf("Owned(%q, %d) = %v，期望 %v", c.in, c.uid, got, c.owned)
			}
		})
	}
}
