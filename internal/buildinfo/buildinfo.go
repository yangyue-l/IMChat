// Package buildinfo 承载编译期注入的版本信息。
//
// 变量默认值是「未注入」的占位而不是空串：`go run`、`go test`、IDE 直接
// 构建都不经过 make，ldflags 不会生效，此时输出 dev/none 才能一眼看出
// 「这不是正式构建」；空串在日志里只表现为少了一个字段，反而像 bug。
package buildinfo

import "strings"

var (
	// Version 由 `git describe --tags --always --dirty` 注入。
	// 有 tag 时形如 v0.1.0，tag 之后又有提交则带 -3-gabc1234，
	// 工作区有未提交改动则追加 -dirty。
	Version = "dev"
	// Commit 是构建时的短 hash，用来与 Version 里的 g 后缀互相核对 ——
	// 两者不一致说明镜像里的版本串是手工拼的。
	Commit = "none"
)

// Dirty 表示构建时工作区有未提交改动。
//
// 不由 ldflags 单独注入一个布尔：那就成了两个来源，Version 后缀说有改动、
// 注入的标记说没有，排查时反而要判断哪个可信。只认 Version 这一处。
func Dirty() bool {
	return strings.HasSuffix(Version, "-dirty")
}
