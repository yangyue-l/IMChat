package cache

import (
	"context"
	"fmt"
)

// 群成员 ID 列表。读扩散下每次发群消息都要按它扇出，命中率极高，
// 任何成员变更（邀请、退群、踢人、解散）都必须同步失效。

// GroupMemberIDs 读取群成员 ID 列表缓存，TTL 取 config.cache.group_member_ttl。
// 回源拿到空列表时由调用方返回 ErrNotFound，交给 Remember 写空值占位防穿透。
func (c *Cache) GroupMemberIDs(ctx context.Context, gid int64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	return c.Remember(ctx, fmt.Sprintf("%s%d", keyGroupMemberIDs, gid), c.cfg.GroupMemberTTL.Get(), load)
}

// DelGroupMembers 失效群成员缓存。
func (c *Cache) DelGroupMembers(ctx context.Context, gid int64) error {
	return c.rdb.Del(ctx, fmt.Sprintf("%s%d", keyGroupMemberIDs, gid)).Err()
}
