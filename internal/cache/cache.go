// Package cache 封装 Redis 访问：Key 规划、缓存击穿/穿透/雪崩防护、计数与在线状态。
//
// 文件按业务域划分，一个文件只放一类 Key 的操作：
//
//	cache.go     门面结构、Key 规划总表、通用读穿逻辑 Remember
//	user.go      用户资料、登录失败计数
//	relation.go  好友关系、拉黑关系判定结果
//	group.go     群成员列表
//	message.go   会话序号、消息幂等、未读计数、已读序号
//	presence.go  设备在线状态
//	moment.go    朋友圈时间线
//	token.go     登出令牌黑名单
//	rate.go      滑动窗口限流
//	lua.go       Lua 脚本集中定义
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"imchat/internal/config"
)

// Key 前缀集中定义，避免各处硬编码字符串。
// 这张表是 Redis 命名空间的唯一索引，新增 Key 必须先在这里登记。
const (
	keyUser           = "im:user:"        // string 用户资料
	keyRelFriend      = "im:rel:friend:"  // string 好友关系判定结果
	keyRelBlocked     = "im:rel:blocked:" // string 拉黑关系判定结果
	keyGroupMemberIDs = "im:gmemberids:"  // string 群成员 ID 列表
	keySeq            = "im:seq:"         // string 会话序号
	keyDedup          = "im:dedup:"       // string 客户端消息幂等
	keyUnread         = "im:unread:"      // hash   conv_id -> 未读数
	keyRead           = "im:read:"        // hash   conv_id -> 已读 seq
	keyOnline         = "im:online:"      // zset   设备 ID -> 设备自身过期时刻
	keyFeed           = "im:feed:"        // zset   moment_id -> 发布时间戳
	keyTokenDeny      = "im:token:deny:"  // string 登出令牌摘要
	keyRate           = "im:rate:"        // zset   滑动窗口的放行记录
	keyLoginFail      = "im:loginfail:"   // string 登录失败计数
)

// ErrNotFound 表示缓存与数据源均无该数据。
var ErrNotFound = errors.New("数据不存在")

// nullMarker 用于空值占位，防缓存穿透。
const nullMarker = "\x00"

// Cache 是 Redis 访问门面。
type Cache struct {
	rdb *redis.Client
	cfg config.CacheConfig
	sf  singleflight.Group
}

// New 创建缓存门面。
func New(rdb *redis.Client, cfg config.CacheConfig) *Cache {
	return &Cache{rdb: rdb, cfg: cfg}
}

// Client 暴露底层客户端，供少数需要自定义命令的场景使用。
func (c *Cache) Client() *redis.Client { return c.rdb }

// jitter 给过期时间加随机抖动，避免同一批 Key 同时失效造成雪崩。
func (c *Cache) jitter(ttl time.Duration) time.Duration {
	j := c.cfg.TTLJitter.Get()
	if j <= 0 {
		return ttl
	}
	return ttl + time.Duration(rand.Int63n(int64(j)))
}

// Remember 读取缓存，未命中则回源；同 Key 并发回源只放行一个请求（singleflight）。
// 数据源返回 ErrNotFound 时写入空值占位，短 TTL 防穿透。
func (c *Cache) Remember(ctx context.Context, key string, ttl time.Duration, load func(context.Context) ([]byte, error)) ([]byte, error) {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == nil {
		if string(raw) == nullMarker {
			return nil, ErrNotFound
		}
		return raw, nil
	}
	if !errors.Is(err, redis.Nil) {
		// 缓存异常不阻断主流程，直接回源
		return load(ctx)
	}

	v, err, _ := c.sf.Do(key, func() (any, error) {
		// 双重检查：等锁期间可能已被其他请求填充
		if cached, err := c.rdb.Get(ctx, key).Bytes(); err == nil {
			if string(cached) == nullMarker {
				return nil, ErrNotFound
			}
			return cached, nil
		}
		data, err := load(ctx)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				_ = c.rdb.Set(ctx, key, nullMarker, time.Minute).Err()
			}
			return nil, err
		}
		_ = c.rdb.Set(ctx, key, data, c.jitter(ttl)).Err()
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// MarshalJSON 便捷序列化，供 service 层复用。
func MarshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
