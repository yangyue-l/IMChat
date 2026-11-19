package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// metricsBody 渲染一次指标输出，返回文本。
// 只调 writeMetric 这一个包级函数：r.metrics 还要 hub 与
// runtime 计数，为了测格式去凑一个 Router 不值得。
func metricsBody(t *testing.T, render func(*gin.Context)) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	render(c)
	return w.Body.String()
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
