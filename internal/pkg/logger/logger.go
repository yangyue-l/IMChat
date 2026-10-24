// Package logger 基于标准库 log/slog 提供统一日志入口，避免引入额外日志框架。
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// Init 按运行模式创建全局 logger：release 用 JSON（便于采集），debug 用文本（便于阅读）。
func Init(mode string) *slog.Logger {
	level := slog.LevelDebug
	if strings.EqualFold(mode, "release") {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if strings.EqualFold(mode, "release") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}

	l := slog.New(h).With("app", "imchat")
	slog.SetDefault(l)
	return l
}
