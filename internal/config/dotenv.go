package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// dotEnvFile 是本地开发用的键值文件名，相对进程工作目录解析。
const dotEnvFile = ".env"

// loadDotEnv 把 .env 里的键值注入进程环境变量。
func loadDotEnv(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	envMap, err := godotenv.Unmarshal(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		return fmt.Errorf("解析 %s 失败: %w", path, err)
	}

	for key, value := range envMap {
		// 缺 '=' 的行由 godotenv 自己报错（unexpected character ... in variable name），
		// 这里挡的是 "=value" 这类空键：它能解析成功，却被归到空键上，
		// 接着 os.Setenv("") 会抛一句和配置毫无关系的系统错误。
		if key == "" {
			return fmt.Errorf("%s 存在无法解析的行：键名不能为空", path)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("设置环境变量 %s 失败: %w", key, err)
		}
	}
	return nil
}
