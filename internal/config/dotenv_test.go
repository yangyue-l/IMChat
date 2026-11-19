package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnv 把正文写进临时目录的 .env 并返回路径。
func writeEnv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时 .env 失败: %v", err)
	}
	return path
}

// unsetForTest 删除指定环境变量，并在用例结束后还原。
// 不能复用 t.Setenv("")：那会让 LookupEnv 返回 true，loadDotEnv 按
// 「真实变量优先」直接跳过，用例就测不到注入了。
func unsetForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, existed := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(k, old)
				return
			}
			_ = os.Unsetenv(k)
		})
	}
}

// TestLoadDotEnvParsesForms 固化 .env.example 里承诺的写法能跑通。
// 这些能力现在由 godotenv 提供，用一条用例守住，升级时不至于悄悄漂掉。
func TestLoadDotEnvParsesForms(t *testing.T) {
	unsetForTest(t,
		"IMCHAT_TEST_DOTENV_PLAIN", "IMCHAT_TEST_DOTENV_DOUBLE",
		"IMCHAT_TEST_DOTENV_EXPORTED", "IMCHAT_TEST_DOTENV_EMPTY",
	)

	path := writeEnv(t, `
# 整行注释
IMCHAT_TEST_DOTENV_PLAIN=hello
IMCHAT_TEST_DOTENV_DOUBLE="quoted value"
export IMCHAT_TEST_DOTENV_EXPORTED=from-shell
IMCHAT_TEST_DOTENV_EMPTY=
`)

	if err := loadDotEnv(path); err != nil {
		t.Fatalf("解析合法 .env 不应报错: %v", err)
	}

	want := map[string]string{
		"IMCHAT_TEST_DOTENV_PLAIN":    "hello",
		"IMCHAT_TEST_DOTENV_DOUBLE":   "quoted value", // 成对引号被剥掉
		"IMCHAT_TEST_DOTENV_EXPORTED": "from-shell",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q，期望 %q", k, got, v)
		}
	}
	// 空值必须真的注入成空串，而不是「没设」
	if v, ok := os.LookupEnv("IMCHAT_TEST_DOTENV_EMPTY"); !ok || v != "" {
		t.Errorf("空值应注入为空串，实际 value=%q exists=%v", v, ok)
	}
}

// TestLoadDotEnvInlineCommentTruncates 固化「值里的 # 会被当注释」这条行为。
// dotenv.go 的注释里警告过：密钥含 # 必须加引号，这里把两种写法都钉住。
func TestLoadDotEnvInlineCommentTruncates(t *testing.T) {
	unsetForTest(t, "IMCHAT_TEST_DOTENV_BARE_HASH", "IMCHAT_TEST_DOTENV_QUOTED_HASH")

	path := writeEnv(t, `
IMCHAT_TEST_DOTENV_BARE_HASH=abc # 行内注释
IMCHAT_TEST_DOTENV_QUOTED_HASH="abc # def"
`)
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv 失败: %v", err)
	}

	if got := os.Getenv("IMCHAT_TEST_DOTENV_BARE_HASH"); got != "abc" {
		t.Errorf("未加引号时应在「空白+#」处截断，得到 %q", got)
	}
	if got := os.Getenv("IMCHAT_TEST_DOTENV_QUOTED_HASH"); got != "abc # def" {
		t.Errorf("加引号后 # 应原样保留，得到 %q", got)
	}
}

func TestLoadDotEnvKeepsExistingVar(t *testing.T) {
	// 真实环境变量优先：容器 / CI 注入的值不能被仓库里的 .env 顶掉
	t.Setenv("IMCHAT_TEST_DOTENV_KEEP", "from-real-env")

	path := writeEnv(t, "IMCHAT_TEST_DOTENV_KEEP=from-dotenv\n")
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv 失败: %v", err)
	}

	if got := os.Getenv("IMCHAT_TEST_DOTENV_KEEP"); got != "from-real-env" {
		t.Errorf("已存在的环境变量被覆盖了，得到 %q", got)
	}
}

func TestLoadDotEnvHandlesBOMAndCRLF(t *testing.T) {
	// Windows 记事本会写 BOM + CRLF。CRLF 由 godotenv 自己归一，
	// BOM 由 loadDotEnv 剥 —— 这条用例守的就是那行 TrimPrefix。
	unsetForTest(t, "IMCHAT_TEST_DOTENV_BOM", "IMCHAT_TEST_DOTENV_CRLF")

	path := writeEnv(t, "\ufeffIMCHAT_TEST_DOTENV_BOM=v1\r\nIMCHAT_TEST_DOTENV_CRLF=v2\r\n")
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv 失败: %v", err)
	}

	for k, want := range map[string]string{
		"IMCHAT_TEST_DOTENV_BOM":  "v1",
		"IMCHAT_TEST_DOTENV_CRLF": "v2",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q，期望 %q", k, got, want)
		}
	}
}

func TestLoadDotEnvMissingFileIsNotAnError(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), "not-exist.env")); err != nil {
		t.Errorf("文件不存在应静默跳过，实际报错: %v", err)
	}
}

func TestLoadDotEnvRejectsBadLines(t *testing.T) {
	// 两类错误来自不同地方，分别断言：
	//   - 缺 '=' 的行：godotenv 自己报「unexpected character」，文案归库管，只断言必须失败；
	//   - "=value"：能解析成功但归到空键，这层护栏是 loadDotEnv 补的。
	// 换库的代价是没有行号 —— 换来的是不必自己维护一套解析器。
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺少等号", "# 注释行\nIMCHAT_TEST_DOTENV_BAD\n", ""},
		{"键名为空", "=value\n", "键名不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := loadDotEnv(writeEnv(t, c.body))
			if err == nil {
				t.Fatal("格式错误应当报错，否则整行会被静默忽略")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestLoadAppliesDotEnvFile 验证 Load 真的接上了 .env，
// 且 .env 的优先级高于 yaml、低于真实环境变量。
func TestLoadAppliesDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(minimalYAML), 0o600); err != nil {
		t.Fatalf("写 config.yaml 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("IMCHAT_OSS_BUCKET=from-dotenv\n"), 0o600); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}
	t.Chdir(dir)

	t.Run(".env 覆盖 yaml", func(t *testing.T) {
		unsetForTest(t, "IMCHAT_OSS_BUCKET")

		cfg, err := Load("config.yaml")
		if err != nil {
			t.Fatalf("Load 失败: %v", err)
		}
		if cfg.Upload.OSS.Bucket != "from-dotenv" {
			t.Errorf("bucket = %q，期望 from-dotenv（.env 应覆盖 yaml 里的 imchat-test）", cfg.Upload.OSS.Bucket)
		}
	})

	t.Run("真实环境变量压过 .env", func(t *testing.T) {
		t.Setenv("IMCHAT_OSS_BUCKET", "from-real-env")

		cfg, err := Load("config.yaml")
		if err != nil {
			t.Fatalf("Load 失败: %v", err)
		}
		if cfg.Upload.OSS.Bucket != "from-real-env" {
			t.Errorf("bucket = %q，期望 from-real-env", cfg.Upload.OSS.Bucket)
		}
	})
}
