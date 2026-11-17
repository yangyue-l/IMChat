package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"imchat/internal/buildinfo"
)

// metricsBody 渲染一次指标输出，返回文本。
// 只调 writeBuildInfo / writeMetric 这两个包级函数：r.metrics 还要 hub 与
// runtime 计数，为了测格式去凑一个 Router 不值得。
func metricsBody(t *testing.T, render func(*gin.Context)) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	render(c)
	return w.Body.String()
}

// withBuildInfo 临时替换版本信息，测试结束后还原，避免用例之间互相污染。
func withBuildInfo(t *testing.T, version, commit string) {
	t.Helper()
	oldV, oldC := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = version, commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldV, oldC })
}

func TestWriteBuildInfoFormat(t *testing.T) {
	withBuildInfo(t, "v0.1.0", "abc1234")
	out := metricsBody(t, writeBuildInfo)

	// 值恒为 1、信息全在标签里，这样版本升级不会产生新的时间序列。
	want := `imchat_build_info{version="v0.1.0",commit="abc1234",dirty="false"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("输出缺少 %q，实际:\n%s", want, out)
	}
	if !strings.Contains(out, "# TYPE imchat_build_info gauge") {
		t.Fatalf("应声明 gauge 类型，实际:\n%s", out)
	}
	if !strings.HasPrefix(out, "# HELP imchat_build_info ") {
		t.Fatalf("HELP 行必须紧跟指标名，实际:\n%s", out)
	}
}

func TestWriteBuildInfoDirty(t *testing.T) {
	// -dirty 后缀是 git describe 表达「工作区有未提交改动」的方式，只能从
	// Version 里读，别再单独注入一个布尔标记。
	withBuildInfo(t, "v0.1.0-3-gabc1234-dirty", "abc1234")
	out := metricsBody(t, writeBuildInfo)
	if !strings.Contains(out, `dirty="true"`) {
		t.Fatalf("带 -dirty 后缀时应输出 dirty=\"true\"，实际:\n%s", out)
	}
}

func TestWriteBuildInfoDefaults(t *testing.T) {
	// 未经 make build 的构建（go run / IDE）必须能一眼看出不是正式版本。
	withBuildInfo(t, "dev", "none")
	out := metricsBody(t, writeBuildInfo)
	if !strings.Contains(out, `version="dev",commit="none",dirty="false"`) {
		t.Fatalf("默认值输出不符合预期:\n%s", out)
	}
}

func TestEscapeLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v0.1.0", "v0.1.0"},
		{"", ""},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{`a\"b`, `a\\\"b`}, // 反向引用与引号同时出现时不能互相吃掉
	}
	for _, c := range cases {
		if got := escapeLabel(c.in); got != c.want {
			t.Errorf("escapeLabel(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestWriteMetricType(t *testing.T) {
	// _total 类累计值必须是 counter，声明成 gauge 会让 rate() 在重启后算出负增长。
	out := metricsBody(t, func(c *gin.Context) {
		writeMetric(c, "imchat_frames_sent_total", "累计写出的下行帧数", metricCounter, 42)
	})
	if !strings.Contains(out, "# TYPE imchat_frames_sent_total counter") {
		t.Fatalf("累计值应声明为 counter，实际:\n%s", out)
	}
	if !strings.Contains(out, "imchat_frames_sent_total 42\n") {
		t.Fatalf("样本行格式不对，实际:\n%s", out)
	}

	out = metricsBody(t, func(c *gin.Context) {
		writeMetric(c, "imchat_online_connections", "当前在线连接数", metricGauge, 7)
	})
	if !strings.Contains(out, "# TYPE imchat_online_connections gauge") {
		t.Fatalf("瞬时值应声明为 gauge，实际:\n%s", out)
	}
}
