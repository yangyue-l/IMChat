// Package jsonx 提供宽松的 JSON 解码类型。
// 前端 JS 的 int64 会被序列化成字符串（避免精度丢失），
// 标准库的 `,string` 标签只对标量生效，无法用于切片，因此这里自实现。
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Int64List 同时接受 ["123","456"]、[123,456] 与混合形式。
type Int64List []int64

// UnmarshalJSON 实现宽松解析。
func (l *Int64List) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*l = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		n, err := parseInt64(item)
		if err != nil {
			return err
		}
		out = append(out, n)
	}
	*l = out
	return nil
}

// parseInt64 兼容带引号与不带引号的数字。
func parseInt64(raw json.RawMessage) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) >= 2 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0, err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("无法解析数字 %q: %w", s, err)
		}
		return n, nil
	}
	n, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析数字 %s: %w", string(trimmed), err)
	}
	return n, nil
}
