package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"imchat/internal/config"
)

// NewRedis 创建 Redis 客户端。
// 连接池按「峰值并发连接数」给足，并设置较短的读写超时，避免慢节点拖垮整个服务。
func NewRedis(cfg config.RedisConfig) (*redis.Client, error) {
	cli := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Password:        cfg.Password,
		DB:              cfg.DB,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		DialTimeout:     cfg.DialTimeout.Get(),
		ReadTimeout:     cfg.ReadTimeout.Get(),
		WriteTimeout:    cfg.WriteTimeout.Get(),
		PoolTimeout:     cfg.ReadTimeout.Get() + cfg.WriteTimeout.Get(),
		ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 30 * time.Minute,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := cli.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}
	return cli, nil
}
