package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"imchat/internal/cache"
	"imchat/internal/media"
	"imchat/internal/model"
	"imchat/internal/pkg/resp"
)

// usernamePattern 限制用户名可用字符，避免不可见字符与注入风险。
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`)

// 登录防撞库参数。
const (
	maxLoginFailures = 5
	loginFailWindow  = 5 * time.Minute
)

// UserService 用户与认证相关业务。
type UserService struct {
	deps Deps
	rel  *relation
}

// RegisterReq 注册参数。
type RegisterReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

// LoginReq 登录参数。
type LoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Device   string `json:"device"`
}

// LoginResult 登录成功后返回给客户端的信息。
type LoginResult struct {
	Token     string           `json:"token"`
	ExpiresIn int64            `json:"expires_in"` // 秒
	User      model.PublicUser `json:"user"`
}

// UserSearchItem 搜索结果，附带与当前用户的好友关系。
//
// Username 在这里显式回传：搜索本身是按账号或昵称匹配的，结果里不带登录名
// 就分不清两个同名用户，用户也没法确认搜到的是不是想找的那个人。
// 除搜索之外的任何投影都不带它。
type UserSearchItem struct {
	model.PublicUser
	Username string `json:"username"`
	IsFriend bool   `json:"is_friend"`
}

// UpdateProfileReq 可修改的资料字段，指针用于区分「未传」与「传空」。
type UpdateProfileReq struct {
	Nickname  *string `json:"nickname"`
	Avatar    *string `json:"avatar"`
	Gender    *int8   `json:"gender"`
	Signature *string `json:"signature"`
}

// Register 注册并直接签发令牌，省去一次登录往返。
func (s *UserService) Register(ctx context.Context, req RegisterReq) (*LoginResult, error) {
	req.Username = strings.TrimSpace(req.Username)
	if !usernamePattern.MatchString(req.Username) {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "用户名需为 3-20 位字母、数字或下划线")
	}
	if len(req.Password) < 6 || len(req.Password) > 32 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "密码长度需为 6-32 位")
	}
	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = req.Username
	}
	if len([]rune(nickname)) > 16 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "昵称最长 16 个字符")
	}

	exist, err := s.deps.Users.GetByUsername(ctx, req.Username)
	if err != nil {
		return nil, err
	}
	if exist != nil {
		return nil, resp.WithMsg(resp.ErrConflict, "用户名已被占用")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("密码加密失败: %w", err)
	}

	u := &model.User{
		Username:     req.Username,
		PasswordHash: string(hash),
		Nickname:     nickname,
		Avatar:       defaultAvatar(req.Username),
	}
	if err := s.deps.Users.Create(ctx, u); err != nil {
		return nil, err
	}
	return s.issueToken(ctx, u, "web")
}

// Login 登录校验。连续失败 5 次锁定 5 分钟，抵御撞库。
func (s *UserService) Login(ctx context.Context, req LoginReq) (*LoginResult, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "请输入用户名与密码")
	}

	// 失败 5 次锁 5 分钟。计数用 Lua 保证「自增 + 续期」原子，避免 key 无 TTL 导致永久锁号。
	if s.deps.Cache.LoginFailures(ctx, username) >= maxLoginFailures {
		return nil, resp.WithMsg(resp.ErrTooManyRequests, "密码错误次数过多，请 5 分钟后再试")
	}

	u, err := s.deps.Users.GetByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		// 用户不存在也计数，避免被用来枚举用户名
		_, _ = s.deps.Cache.RecordLoginFailure(ctx, username, loginFailWindow)
		return nil, resp.WithMsg(resp.ErrUnauthorized, "用户名或密码错误")
	}
	if u.Status == model.UserStatusDisabled {
		return nil, resp.WithMsg(resp.ErrForbidden, "账号已被禁用")
	}
	_ = s.deps.Cache.ResetLoginFailures(ctx, username)

	device := req.Device
	if device == "" {
		device = "web"
	}
	return s.issueToken(ctx, u, device)
}

func (s *UserService) issueToken(ctx context.Context, u *model.User, device string) (*LoginResult, error) {
	token, err := s.deps.JWT.Generate(u.ID, device)
	if err != nil {
		return nil, fmt.Errorf("签发令牌失败: %w", err)
	}
	now := time.Now()
	_ = s.deps.Users.UpdateFields(ctx, u.ID, map[string]any{"last_active_at": now})
	return &LoginResult{
		Token:     token,
		ExpiresIn: int64(s.deps.JWT.Expire().Seconds()),
		User:      u.ToPublic(),
	}, nil
}

// Logout 将令牌加入黑名单，剩余有效期内不可再用。
func (s *UserService) Logout(ctx context.Context, token string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return s.deps.Cache.DenyToken(ctx, token, ttl)
}

// Profile 获取用户资料，缓存优先。
func (s *UserService) Profile(ctx context.Context, uid int64) (*model.PublicUser, error) {
	raw, err := s.deps.Cache.LoadUser(ctx, uid, func(ctx context.Context) ([]byte, error) {
		u, err := s.deps.Users.GetByID(ctx, uid)
		if err != nil {
			return nil, err
		}
		if u == nil {
			// 必须回 cache.ErrNotFound 这个哨兵：Remember 只认它才会写空值占位，
			// 回 resp.ErrNotFound 的话「查一个不存在的 uid」每次都会打穿到 DB。
			return nil, cache.ErrNotFound
		}
		return json.Marshal(u.ToPublic())
	})
	if errors.Is(err, cache.ErrNotFound) {
		return nil, resp.WithMsg(resp.ErrNotFound, "用户不存在")
	}
	if err != nil {
		return nil, err
	}
	var p model.PublicUser
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Online 查询某用户是否有任意设备在线。
//
// 可见性对齐朋友圈（MomentService.UserMoments）：先看拉黑、再看好友。
// 只判好友是不够的 —— 拉黑不解除好友关系，那样「拉黑之后对方还能看到我此刻在线」，
// 与拉黑本身的语义相悖。不加限制更糟：遍历 uid 就能探测谁在线、什么时段在线，
// 等于一份公开的作息画像。两个关系判定都走 5 分钟缓存，不会额外压库。
func (s *UserService) Online(ctx context.Context, viewerID, targetID int64) (bool, error) {
	if s.deps.Presence == nil {
		// 未装配在线能力（单测、只装了 DB 的工具链）时按离线降级，不 panic。
		// 放在关系校验之前：能力都没有，没必要白查一次好友关系。
		return false, nil
	}
	if viewerID != targetID {
		blocked, err := s.rel.EitherBlocked(ctx, viewerID, targetID)
		if err != nil {
			return false, err
		}
		if blocked {
			return false, resp.WithMsg(resp.ErrForbidden, "只能查看好友的在线状态")
		}
		isFriend, err := s.rel.AreFriends(ctx, viewerID, targetID)
		if err != nil {
			return false, err
		}
		if !isFriend {
			return false, resp.WithMsg(resp.ErrForbidden, "只能查看好友的在线状态")
		}
	}

	online, err := s.deps.Presence.Online([]int64{targetID})
	if err != nil {
		return false, err
	}
	return online[targetID], nil
}

// UpdateProfile 修改资料并失效缓存。
func (s *UserService) UpdateProfile(ctx context.Context, uid int64, req UpdateProfileReq) (*model.PublicUser, error) {
	fields := map[string]any{}
	// 旧头像：本次改头像且新旧不同时才需要回收（同一地址重复保存不能删）
	var oldAvatar string
	if req.Nickname != nil {
		n := strings.TrimSpace(*req.Nickname)
		if n == "" || len([]rune(n)) > 16 {
			return nil, resp.WithMsg(resp.ErrInvalidParam, "昵称需为 1-16 个字符")
		}
		fields["nickname"] = n
	}
	if req.Avatar != nil {
		// 前端回传的是上传接口给的绝对地址，入库存 key：换 CDN 时这张表不用回刷
		key := media.Key(*req.Avatar)
		if len(key) > maxAvatarBytes {
			return nil, resp.WithMsg(resp.ErrInvalidParam, "头像地址过长")
		}
		fields["avatar"] = key
		// 换头像要回收旧对象，旧值先取出来。取不到不阻断：清理是附带动作，
		// 退化成留一个孤儿，好过让用户改不了资料。
		if old, err := s.Profile(ctx, uid); err == nil && old != nil {
			// 同一个地址重复保存时新旧 key 相同，那个对象仍被引用，不能删
			if oldKey := media.Key(old.Avatar); oldKey != "" && oldKey != key {
				oldAvatar = oldKey
			}
		}
	}
	if req.Gender != nil {
		if *req.Gender < 0 || *req.Gender > 2 {
			return nil, resp.WithMsg(resp.ErrInvalidParam, "性别取值非法")
		}
		fields["gender"] = *req.Gender
	}
	if req.Signature != nil {
		if len([]rune(*req.Signature)) > 80 {
			return nil, resp.WithMsg(resp.ErrInvalidParam, "签名最长 80 个字符")
		}
		fields["signature"] = *req.Signature
	}
	if len(fields) == 0 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "没有需要更新的字段")
	}
	if err := s.deps.Users.UpdateFields(ctx, uid, fields); err != nil {
		return nil, err
	}
	_ = s.deps.Cache.DelUser(ctx, uid)
	// 旧头像在库里已经不被引用了，回收它。放在写成功之后，理由见 dropObjects。
	if oldAvatar != "" {
		dropObjects(ctx, s.deps.Files, uid, oldAvatar)
	}
	return s.Profile(ctx, uid)
}

// Search 按用户名/昵称搜索用户，排除自己，并标注是否已是好友。
func (s *UserService) Search(ctx context.Context, uid int64, keyword string, limit int) ([]UserSearchItem, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "请输入搜索关键词")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	list, err := s.deps.Users.Search(ctx, keyword, limit+1)
	if err != nil {
		return nil, err
	}
	friendIDs, err := s.deps.Friends.FriendIDs(ctx, uid)
	if err != nil {
		return nil, err
	}
	friendSet := make(map[int64]struct{}, len(friendIDs))
	for _, id := range friendIDs {
		friendSet[id] = struct{}{}
	}
	out := make([]UserSearchItem, 0, len(list))
	for i := range list {
		if list[i].ID == uid || list[i].Status != model.UserStatusNormal {
			continue
		}
		_, isFriend := friendSet[list[i].ID]
		out = append(out, UserSearchItem{
			PublicUser: list[i].ToPublic(),
			Username:   list[i].Username,
			IsFriend:   isFriend,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// defaultAvatar 用用户名首字母生成占位头像标识，前端据此渲染色块。
func defaultAvatar(username string) string {
	if username == "" {
		return ""
	}
	return "avatar://" + strings.ToUpper(username[:1])
}
