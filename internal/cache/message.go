package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 会话序号、消息幂等、未读计数与已读序号。
//
// 这四者构成消息投递的计数基础：seq 用于排序与未读差值的计算，
// 未读与已读必须成对维护，拆开写会出现「有红点看不到消息」这类状态错乱。

// unreadTTL 未读计数的存活时间。超过这个时间没有任何新消息，未读视为已失效，
// 让 hash 自然回收；读取时若缺失，会按已读序号回源 DB 重算。
const unreadTTL = 30 * 24 * time.Hour

// NextSeq 为会话分配下一个序号。Redis 单线程执行脚本，保证严格递增且无锁竞争。
//
// 返回 0 表示序号键不存在（新会话，或 Redis 丢键后重建），调用方必须先查
// MAX(seq) 再调用 InitSeq。把「探测」与「分配」拆开的用意是：键不存在时不消耗
// 号段，并发请求会一起走到初始化路径，由 InitSeq 的原子抬升收敛到同一段序号。
func (c *Cache) NextSeq(ctx context.Context, convID string) (int64, error) {
	return nextSeqScript.Run(ctx, c.rdb, []string{keySeq + convID}).Int64()
}

// InitSeq 用库中最大 seq 初始化序号键并返回下一个可用序号，用于 Redis 丢键后的恢复。
// floor 传 0 表示会话确实没有历史消息，此时从 1 开始分配。
func (c *Cache) InitSeq(ctx context.Context, convID string, floor int64) (int64, error) {
	return initSeqScript.Run(ctx, c.rdb, []string{keySeq + convID}, floor).Int64()
}

// MarkClientMsg 客户端消息幂等标记：首次写入返回 true，重复返回 false。
func (c *Cache) MarkClientMsg(ctx context.Context, fromID int64, clientMsgID string) (bool, error) {
	return c.rdb.SetNX(ctx, fmt.Sprintf("%s%d:%s", keyDedup, fromID, clientMsgID), 1, 10*time.Minute).Result()
}

// IncrUnread 批量增加未读数，pipeline 把 N 次 HINCRBY 合并为一次往返。
// 每个 key 顺带续期：否则未读 hash 一旦建立就永不过期，用户全部会话记录常驻内存。
// HINCRBY 本身是原子命令，这里不需要 Lua；真正需要保证的是「计数与 TTL 同生」。
func (c *Cache) IncrUnread(ctx context.Context, convID string, uids []int64, delta int64) error {
	if len(uids) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for _, uid := range uids {
		key := fmt.Sprintf("%s%d", keyUnread, uid)
		pipe.HIncrBy(ctx, key, convID, delta)
		pipe.Expire(ctx, key, unreadTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// UnreadCounts 批量读取某用户全部会话未读数。
func (c *Cache) UnreadCounts(ctx context.Context, uid int64) (map[string]int64, error) {
	m, err := c.rdb.HGetAll(ctx, fmt.Sprintf("%s%d", keyUnread, uid)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			out[k] = n
		}
	}
	return out, nil
}

// MarkRead 上报已读位置，原子完成「已读序号只前进」与「未读计数清零」。
//
// 三件事必须一起成立，否则会出现用户看得到的状态错乱：
//   - 已读序号回退，群未读按 seq 差实时计算，会凭空多出未读；
//   - 序号前进了但未读没清，私聊角标会一直挂着；
//   - 清未读失败但序号前进，多端同步时两边看到的未读数不同。
//
// 返回最终生效的序号（调用方用它做 DB 落库与回执，保证两边一致）。
func (c *Cache) MarkRead(ctx context.Context, uid int64, convID string, seq int64) (int64, error) {
	res, err := markReadScript.Run(ctx, c.rdb,
		[]string{fmt.Sprintf("%s%d", keyRead, uid), fmt.Sprintf("%s%d", keyUnread, uid)},
		convID, seq).Slice()
	if err != nil {
		return seq, err
	}
	if len(res) == 0 {
		return seq, nil
	}
	final, _ := res[0].(int64)
	return final, nil
}

// ReadSeqs 批量读取用户在全部会话中的已读序号。
// 读扩散下群的未读数 = 群最新 seq - 已读 seq，因此需要一次性拿到全部已读位置。
func (c *Cache) ReadSeqs(ctx context.Context, uid int64) (map[string]int64, error) {
	m, err := c.rdb.HGetAll(ctx, fmt.Sprintf("%s%d", keyRead, uid)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		var n int64
		// 已读序号 0 是有效值（刚入群、群内还没有消息），必须保留
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			out[k] = n
		}
	}
	return out, nil
}

// SetReadSeqIfAbsent 只在尚无已读记录时写入，用于新成员入群对齐已读位置。
// 用 HSETNX 保证不会把老成员更靠后的已读位置覆盖回去。
func (c *Cache) SetReadSeqIfAbsent(ctx context.Context, uid int64, convID string, seq int64) error {
	return c.rdb.HSetNX(ctx, fmt.Sprintf("%s%d", keyRead, uid), convID, seq).Err()
}

// GetReadSeq 读取用户在会话内的已读序号。
func (c *Cache) GetReadSeq(ctx context.Context, uid int64, convID string) (int64, error) {
	n, err := c.rdb.HGet(ctx, fmt.Sprintf("%s%d", keyRead, uid), convID).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}
