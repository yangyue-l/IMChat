package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"imchat/internal/cache"
	"imchat/internal/model"
	"imchat/internal/repository"
)

// relation 收敛关系校验逻辑：好友、拉黑、群成员。
// 这些判断在每条消息、每次朋友圈读取时都会发生，必须有缓存兜住。
type relation struct {
	cache   *cache.Cache
	friends *repository.FriendRepo
	groups  *repository.GroupRepo
	users   *repository.UserRepo
}

const (
	relFriendKey    = "im:rel:friend:%d:%d"
	relBlockedKey   = "im:rel:blocked:%d:%d"
	groupMembersKey = "im:gmemberids:%d"
)

// AreFriends 判断好友关系，结果缓存 5 分钟。
func (r *relation) AreFriends(ctx context.Context, a, b int64) (bool, error) {
	if a == b {
		return true, nil
	}
	raw, err := r.cache.Remember(ctx, fmt.Sprintf(relFriendKey, a, b), 5*time.Minute, func(ctx context.Context) ([]byte, error) {
		ok, err := r.friends.IsFriend(ctx, a, b)
		if err != nil {
			return nil, err
		}
		if ok {
			return []byte("1"), nil
		}
		return []byte("0"), nil
	})
	if err != nil {
		return false, err
	}
	return string(raw) == "1", nil
}

// EitherBlocked 判断双方是否存在拉黑关系（任一方向）。
func (r *relation) EitherBlocked(ctx context.Context, a, b int64) (bool, error) {
	if a == b {
		return false, nil
	}
	raw, err := r.cache.Remember(ctx, fmt.Sprintf(relBlockedKey, a, b), 5*time.Minute, func(ctx context.Context) ([]byte, error) {
		ok, err := r.friends.EitherBlocked(ctx, a, b)
		if err != nil {
			return nil, err
		}
		if ok {
			return []byte("1"), nil
		}
		return []byte("0"), nil
	})
	if err != nil {
		return false, err
	}
	return string(raw) == "1", nil
}

// InvalidateFriend 关系变更时清理双向缓存。
func (r *relation) InvalidateFriend(ctx context.Context, a, b int64) {
	cli := r.cache.Client()
	_ = cli.Del(ctx, fmt.Sprintf(relFriendKey, a, b), fmt.Sprintf(relFriendKey, b, a),
		fmt.Sprintf(relBlockedKey, a, b), fmt.Sprintf(relBlockedKey, b, a)).Err()
}

// MemberIDs 群成员 ID 列表，带缓存，用于消息扇出。
func (r *relation) MemberIDs(ctx context.Context, gid int64) ([]int64, error) {
	raw, err := r.cache.Remember(ctx, fmt.Sprintf(groupMembersKey, gid), 5*time.Minute, func(ctx context.Context) ([]byte, error) {
		ids, err := r.groups.MemberIDs(ctx, gid)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, cache.ErrNotFound
		}
		return json.Marshal(ids)
	})
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// Member 查询群成员信息（含角色、禁言状态）。
func (r *relation) Member(ctx context.Context, gid, uid int64) (*model.GroupMember, error) {
	return r.groups.GetMember(ctx, gid, uid)
}

// InvalidateGroupMembers 群成员变化时清缓存。
func (r *relation) InvalidateGroupMembers(ctx context.Context, gid int64) {
	_ = r.cache.Client().Del(ctx, fmt.Sprintf(groupMembersKey, gid)).Err()
}

// UserMap 批量取用户信息并转 map，供列表补全昵称头像，避免 N+1。
func (r *relation) UserMap(ctx context.Context, ids []int64) (map[int64]model.PublicUser, error) {
	out := make(map[int64]model.PublicUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list, err := r.users.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		out[list[i].ID] = list[i].ToPublic()
	}
	return out, nil
}
