package cache

import (
	"context"
	"fmt"
)

// 好友与拉黑的关系判定结果。每条消息、每次朋友圈可见性校验都要问一次，
// 结果只有「是/否」两态，用最省的 string "1" / "0" 存，不值得上 JSON。

// FriendRelation 读取 a 与 b 的好友关系。
// 存两个方向各一条：a→b 命中后，b→a 的查询不必再回源。
func (c *Cache) FriendRelation(ctx context.Context, a, b int64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return c.Remember(ctx, fmt.Sprintf("%s%d:%d", keyRelFriend, a, b), c.cfg.FriendTTL.Get(), load)
}

// BlockedRelation 读取 a 与 b 的拉黑关系（任一方向），语义对齐 repository.EitherBlocked。
func (c *Cache) BlockedRelation(ctx context.Context, a, b int64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return c.Remember(ctx, fmt.Sprintf("%s%d:%d", keyRelBlocked, a, b), c.cfg.FriendTTL.Get(), load)
}

// DelRelations 关系变更时一次清掉双向的好友与拉黑判定。
//
// 四条必须一起删：拉黑键存的是「任一方向」的合并结论，只删 a:b 会让 b:a
// 继续命中旧的否定结果，表现就是「拉黑之后对方还能发消息」。
func (c *Cache) DelRelations(ctx context.Context, a, b int64) error {
	keys := make([]string, 0, 4)
	for _, p := range [][2]int64{{a, b}, {b, a}} {
		keys = append(keys,
			fmt.Sprintf("%s%d:%d", keyRelFriend, p[0], p[1]),
			fmt.Sprintf("%s%d:%d", keyRelBlocked, p[0], p[1]))
	}
	return c.rdb.Del(ctx, keys...).Err()
}
