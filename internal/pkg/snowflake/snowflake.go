// Package snowflake 提供全局分布式 ID 生成器。
// ID 为 int64，序列化到 JSON 时通过 `,string` 标签输出字符串，避免前端精度丢失。
package snowflake

import (
	"fmt"
	"sync"

	sf "github.com/bwmarrin/snowflake"
)

var (
	mu   sync.RWMutex
	node *sf.Node
)

// Init 初始化节点，nodeID 取值 0-1023，多实例部署需保证唯一。
func Init(nodeID int64) error {
	n, err := sf.NewNode(nodeID)
	if err != nil {
		return fmt.Errorf("初始化雪花节点失败: %w", err)
	}
	mu.Lock()
	node = n
	mu.Unlock()
	return nil
}

// Next 返回下一个 ID；未初始化时 panic，属于启动期编程错误。
func Next() int64 {
	mu.RLock()
	n := node
	mu.RUnlock()
	if n == nil {
		panic("snowflake 未初始化")
	}
	return n.Generate().Int64()
}
