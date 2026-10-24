package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 朋友圈时间线。发布时写扩散到好友的 zset，读取时先查 zset，
// 命中不足再由 service 回源 MySQL（拉模式降级）。

// PushFeed 把动态写入指定用户的时间线（写扩散），并裁剪长度控制内存。
// 写入、裁剪、续期必须原子：任何一步单独失败都会留下「无 TTL 且不断增长」的 zset。
func (c *Cache) PushFeed(ctx context.Context, uid int64, momentID int64, ts int64, maxLen int64) error {
	return pushFeedScript.Run(ctx, c.rdb,
		[]string{fmt.Sprintf("%s%d", keyFeed, uid)},
		momentID, ts, maxLen, int64(7*24*time.Hour/time.Millisecond)).Err()
}

// FeedIDs 读取时间线中的动态 ID（按时间倒序），供拉模式回源前快速命中。
func (c *Cache) FeedIDs(ctx context.Context, uid int64, before int64, limit int64) ([]int64, error) {
	var (
		res []string
		err error
	)
	key := fmt.Sprintf("%s%d", keyFeed, uid)
	if before > 0 {
		res, err = c.rdb.ZRevRangeByScore(ctx, key, &redis.ZRangeBy{
			Max: fmt.Sprintf("(%d", before), Min: "-inf", Count: limit,
		}).Result()
	} else {
		res, err = c.rdb.ZRevRange(ctx, key, 0, limit-1).Result()
	}
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(res))
	for _, s := range res {
		var id int64
		if _, err := fmt.Sscanf(s, "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
