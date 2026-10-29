package service

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"imchat/internal/model"
	"imchat/internal/pkg/jsonx"
	"imchat/internal/pkg/resp"
)

// GroupService 群聊业务。
type GroupService struct {
	deps Deps
	rel  *relation
}

// CreateGroupReq 建群参数。member_ids 同时兼容字符串与数字形式。
type CreateGroupReq struct {
	Name      string          `json:"name"`
	MemberIDs jsonx.Int64List `json:"member_ids"`
}

// UpdateGroupReq 群资料修改参数，指针区分未传与传空。
type UpdateGroupReq struct {
	Name   *string `json:"name"`
	Avatar *string `json:"avatar"`
	Notice *string `json:"notice"`
}

// GroupItem 群列表项。
type GroupItem struct {
	ID          int64     `json:"id,string"`
	Name        string    `json:"name"`
	Avatar      string    `json:"avatar"`
	OwnerID     int64     `json:"owner_id,string"`
	Notice      string    `json:"notice"`
	MemberCount int       `json:"member_count"`
	MaxMember   int       `json:"max_member"`
	MyRole      int8      `json:"my_role"`
	Muted       bool      `json:"muted"`
	CreatedAt   time.Time `json:"created_at"`
}

// GroupMemberItem 群成员项。
type GroupMemberItem struct {
	model.PublicUser
	Role          int8      `json:"role"`
	GroupNickname string    `json:"group_nickname"`
	Muted         bool      `json:"muted"`
	JoinedAt      time.Time `json:"joined_at"`
}

// GroupDetail 群详情。
type GroupDetail struct {
	Group   GroupItem         `json:"group"`
	Members []GroupMemberItem `json:"members"`
}

// MemberPage 群成员分页结果。
type MemberPage struct {
	Total   int               `json:"total"`
	Offset  int               `json:"offset"`
	Limit   int               `json:"limit"`
	Members []GroupMemberItem `json:"members"`
}

// 成员分页的缺省值与上限。上限设 200 是给「一次拉多点少翻页」留空间，
// 再大就等于让客户端用一次请求把 1000 人的群整库拉走。
const (
	defaultMemberPageSize = 50
	maxMemberPageSize     = 200
)

// normalizePage 归一化分页参数。
// limit 传 0/负数一律当没传，按缺省值处理；offset 传负数归零，
// 否则带进 SQL 的负 OFFSET 会被 MySQL 直接报语法错误。
func normalizePage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultMemberPageSize
	}
	if limit > maxMemberPageSize {
		limit = maxMemberPageSize
	}
	return offset, limit
}

// Create 建群。事务内一次性写入群、成员与人数，避免中间态。
func (s *GroupService) Create(ctx context.Context, ownerID int64, req CreateGroupReq) (*GroupItem, error) {
	name := req.Name
	if len([]rune(name)) == 0 || len([]rune(name)) > 20 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "群名称需为 1-20 个字符")
	}
	// 去重并剔除自己，限制单次邀请人数，防止一次写入过大
	seen := make(map[int64]struct{}, len(req.MemberIDs))
	ids := make([]int64, 0, len(req.MemberIDs))
	for _, id := range req.MemberIDs {
		if id <= 0 || id == ownerID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > 20 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "单次最多邀请 20 位成员")
	}
	if len(ids) > 0 {
		users, err := s.rel.UserMap(ctx, ids)
		if err != nil {
			return nil, err
		}
		if len(users) != len(ids) {
			return nil, resp.WithMsg(resp.ErrInvalidParam, "部分成员不存在")
		}
	}

	g := &model.Group{
		Name:        name,
		OwnerID:     ownerID,
		MaxMember:   model.MaxGroupMembers,
		MemberCount: len(ids) + 1,
	}

	// 注意：群 ID 由 BeforeCreate 钩子生成，成员行必须在 Create(g) 之后构建
	err := s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(g).Error; err != nil {
			return err
		}
		now := time.Now()
		rows := make([]model.GroupMember, 0, len(ids)+1)
		rows = append(rows, model.GroupMember{
			GroupID: g.ID, UserID: ownerID, Role: model.GroupRoleOwner, JoinedAt: now,
		})
		for _, id := range ids {
			rows = append(rows, model.GroupMember{
				GroupID: g.ID, UserID: id, Role: model.GroupRoleMember, JoinedAt: now,
			})
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
	})
	if err != nil {
		return nil, err
	}

	s.rel.InvalidateGroupMembers(ctx, g.ID)

	// 读扩散：初始化成员的已读位置。此时群内还没有消息，序号为 0，
	// 有了这条记录，之后的第一条消息才能正常产生未读，而不会被"首次读取对齐"吞掉。
	convID := GroupConvID(g.ID)
	members := make([]int64, 0, len(ids)+1)
	members = append(members, ownerID)
	members = append(members, ids...)
	for _, id := range members {
		_ = s.deps.Cache.SetReadSeqIfAbsent(ctx, id, convID, 0)
		_ = s.deps.Messages.UpsertRead(ctx, id, convID, 0)
	}

	s.notify(ctx, g.ID, "create", map[string]any{"group_id": g.ID, "name": g.Name, "operator": ownerID})
	return s.toItem(g, model.GroupRoleOwner, false), nil
}

// List 我加入的群。
func (s *GroupService) List(ctx context.Context, uid int64) ([]GroupItem, error) {
	members, err := s.deps.Groups.ListUserGroups(ctx, uid, 200)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return []GroupItem{}, nil
	}
	ids := make([]int64, 0, len(members))
	roleMap := make(map[int64]model.GroupMember, len(members))
	for _, m := range members {
		ids = append(ids, m.GroupID)
		roleMap[m.GroupID] = m
	}
	groups, err := s.deps.Groups.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]GroupItem, 0, len(groups))
	for i := range groups {
		if groups[i].Status != model.GroupStatusNormal {
			continue
		}
		m := roleMap[groups[i].ID]
		out = append(out, *s.toItem(&groups[i], m.Role, m.Muted))
	}
	return out, nil
}

// Detail 群详情 + 成员列表，仅成员可见。
func (s *GroupService) Detail(ctx context.Context, uid, gid int64) (*GroupDetail, error) {
	g, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return nil, err
	}
	members, err := s.deps.Groups.ListMembers(ctx, gid)
	if err != nil {
		return nil, err
	}
	items, err := s.memberItems(ctx, members)
	if err != nil {
		return nil, err
	}
	return &GroupDetail{Group: *s.toItem(g, me.Role, me.Muted), Members: items}, nil
}

// Members 分页查看群成员，仅群内成员可见。
//
// 总数取群表冗余的 member_count，不再每页跑一次 count(*)：它与群详情、群列表里的
// member_count 是同一个数，不会出现「头部写 1000 人、分页器说 998 人」的自相矛盾。
// 代价是极端情况下（事务外强杀进程）该计数可能偏大，翻到末尾拿到空页，前端按空页收尾即可。
func (s *GroupService) Members(ctx context.Context, uid, gid int64, offset, limit int) (*MemberPage, error) {
	// 参数先行拦掉：uid 来自令牌、gid 来自路径，走到这里都该是正数，
	// 真出现非正值说明调用链有问题，直接报参数错误，别拿它去查库。
	if uid <= 0 || gid <= 0 {
		return nil, resp.WithMsg(resp.ErrInvalidParam, "用户或群参数非法")
	}
	g, _, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return nil, err
	}
	offset, limit = normalizePage(offset, limit)
	members, err := s.deps.Groups.ListMembersPage(ctx, gid, offset, limit)
	if err != nil {
		return nil, err
	}
	items, err := s.memberItems(ctx, members)
	if err != nil {
		return nil, err
	}
	return &MemberPage{Total: g.MemberCount, Offset: offset, Limit: limit, Members: items}, nil
}

// memberItems 补齐成员的用户资料后组装成返回项。
func (s *GroupService) memberItems(ctx context.Context, members []model.GroupMember) ([]GroupMemberItem, error) {
	items := make([]GroupMemberItem, 0, len(members))
	if len(members) == 0 {
		return items, nil
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	users, err := s.rel.UserMap(ctx, ids)
	if err != nil {
		return nil, err
	}
	return buildMemberItems(members, users), nil
}

// buildMemberItems 把成员行与用户资料拼成返回项，保持入参顺序。
//
// 用户资料缺失（缓存/库被清过、成员行残留）时跳过这个人，而不是塞个只有 uid 的空壳：
// 前端拿不到昵称会渲染成一行空白，反而看不出是谁。
// 空成员也要返回非 nil 切片，否则 JSON 会序列化成 null，前端拿它遍历会直接报错。
func buildMemberItems(members []model.GroupMember, users map[int64]model.PublicUser) []GroupMemberItem {
	items := make([]GroupMemberItem, 0, len(members))
	for i := range members {
		u, ok := users[members[i].UserID]
		if !ok {
			continue
		}
		m := &members[i]
		items = append(items, GroupMemberItem{
			PublicUser: u, Role: m.Role, GroupNickname: m.GroupNickname,
			Muted: m.Muted, JoinedAt: m.JoinedAt,
		})
	}
	return items
}

// Update 修改群资料，仅管理员可操作。
func (s *GroupService) Update(ctx context.Context, uid, gid int64, req UpdateGroupReq) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}
	if !me.IsAdmin() {
		return resp.WithMsg(resp.ErrForbidden, "只有群主或管理员可以修改群资料")
	}
	fields := map[string]any{}
	if req.Name != nil {
		n := *req.Name
		if len([]rune(n)) == 0 || len([]rune(n)) > 20 {
			return resp.WithMsg(resp.ErrInvalidParam, "群名称需为 1-20 个字符")
		}
		fields["name"] = n
	}
	if req.Avatar != nil {
		fields["avatar"] = *req.Avatar
	}
	if req.Notice != nil {
		if len([]rune(*req.Notice)) > 300 {
			return resp.WithMsg(resp.ErrInvalidParam, "群公告最长 300 个字符")
		}
		fields["notice"] = *req.Notice
	}
	if len(fields) == 0 {
		return resp.WithMsg(resp.ErrInvalidParam, "没有需要更新的字段")
	}
	if err := s.deps.Groups.UpdateFields(ctx, gid, fields); err != nil {
		return err
	}
	s.notify(ctx, gid, "update", map[string]any{"group_id": gid, "operator": uid})
	return nil
}

// AddMembers 邀请成员入群。
//
// 整个「查已存在 → 校验上限 → 插入成员 → 更新人数」必须在一个事务里，且持有群行锁：
//   - 上限校验是先读后写，不加锁时两个并发邀请都读到 980 人、都判定不超限，
//     最终写进 1020 人，1000 的上限形同虚设；
//   - 插入与人数自增分开，自增失败会让 member_count 永久偏小，之后所有上限判断都失真。
func (s *GroupService) AddMembers(ctx context.Context, uid, gid int64, memberIDs []int64) (int, error) {
	if _, _, err := s.mustBeMember(ctx, uid, gid); err != nil {
		return 0, err
	}
	seen := make(map[int64]struct{}, len(memberIDs))
	ids := make([]int64, 0, len(memberIDs))
	for _, id := range memberIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, resp.WithMsg(resp.ErrInvalidParam, "没有有效的邀请对象")
	}

	var addedIDs []int64
	var lastSeq int64
	err := s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Groups.WithTx(tx)
		g, err := repo.LockGroup(ctx, gid)
		if err != nil {
			return err
		}
		if g == nil || g.Status != model.GroupStatusNormal {
			return resp.WithMsg(resp.ErrNotFound, "群不存在或已解散")
		}
		exist, err := repo.ExistingMemberIDs(ctx, gid, ids)
		if err != nil {
			return err
		}
		toAdd := make([]int64, 0, len(ids))
		for _, id := range ids {
			if _, ok := exist[id]; !ok {
				toAdd = append(toAdd, id)
			}
		}
		if len(toAdd) == 0 {
			return nil
		}
		if g.MemberCount+len(toAdd) > g.MaxMember {
			return resp.WithMsg(resp.ErrForbidden, "群成员已达上限")
		}

		now := time.Now()
		rows := make([]model.GroupMember, 0, len(toAdd))
		for _, id := range toAdd {
			rows = append(rows, model.GroupMember{GroupID: gid, UserID: id, JoinedAt: now})
		}
		if _, err := repo.AddMembers(ctx, rows); err != nil {
			return err
		}
		if err := repo.SetMemberCount(ctx, gid, g.MemberCount+len(toAdd)); err != nil {
			return err
		}
		addedIDs = toAdd
		lastSeq = g.LastMsgSeq
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(addedIDs) == 0 {
		return 0, nil
	}

	// 读扩散下群未读 = 群最新 seq − 已读 seq。
	// 新成员入群必须把已读位置对齐到当前最新，否则入群前的历史消息会被全部算成未读。
	// HSETNX 保证不覆盖老成员已有记录，DB 侧 GREATEST 保证只前进不后退。
	//
	// 注意这里用 addedIDs 而不是请求里的 memberIDs：后者包含本来就是群成员的人，
	// 给他们重写已读位置属于无意义的脏写。
	convID := GroupConvID(gid)
	for _, id := range addedIDs {
		_ = s.deps.Cache.SetReadSeqIfAbsent(ctx, id, convID, lastSeq)
		_ = s.deps.Messages.UpsertRead(ctx, id, convID, lastSeq)
	}
	s.rel.InvalidateGroupMembers(ctx, gid)
	s.notify(ctx, gid, "member_join", map[string]any{
		"group_id": gid, "operator": uid, "members": addedIDs,
	})
	return len(addedIDs), nil
}

// RemoveMember 退群或踢人。
// 规则：本人可退；群主不可直接退（需转让或解散）；管理员可踢普通成员；群主可踢任何人。
func (s *GroupService) RemoveMember(ctx context.Context, uid, gid, targetID int64) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}

	if targetID == uid {
		if me.Role == model.GroupRoleOwner {
			return resp.WithMsg(resp.ErrForbidden, "群主需先转让群主或解散群")
		}
	} else {
		if !me.IsAdmin() {
			return resp.WithMsg(resp.ErrForbidden, "没有权限移除成员")
		}
		target, err := s.deps.Groups.GetMember(ctx, gid, targetID)
		if err != nil {
			return err
		}
		if target == nil {
			return resp.WithMsg(resp.ErrNotFound, "该成员不在群里")
		}
		if target.Role == model.GroupRoleOwner {
			return resp.WithMsg(resp.ErrForbidden, "不能移除群主")
		}
		if me.Role == model.GroupRoleAdmin && target.Role == model.GroupRoleAdmin {
			return resp.WithMsg(resp.ErrForbidden, "管理员之间不能互相移除")
		}
	}

	removed, err := s.removeMemberTx(ctx, gid, targetID)
	if err != nil {
		return err
	}
	if removed > 0 {
		s.rel.InvalidateGroupMembers(ctx, gid)
		s.notify(ctx, gid, "member_leave", map[string]any{
			"group_id": gid, "operator": uid, "target": targetID,
		})
	}
	return nil
}

// removeMemberTx 删除成员行与人数自减放在同一事务，并持有群行锁。
// 原实现用 `_ = IncrMemberCount` 吞掉错误：一旦失败，人数永久偏大，
// 之后所有入群都会被提前判定为「已达上限」。
func (s *GroupService) removeMemberTx(ctx context.Context, gid, targetID int64) (int64, error) {
	var removed int64
	err := s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := s.deps.Groups.WithTx(tx)
		if _, err := repo.LockGroup(ctx, gid); err != nil {
			return err
		}
		n, err := repo.RemoveMember(ctx, gid, targetID)
		if err != nil || n == 0 {
			return err
		}
		removed = n
		return repo.IncrMemberCount(ctx, gid, -1)
	})
	return removed, err
}

// MuteMember 设置成员禁言。
func (s *GroupService) MuteMember(ctx context.Context, uid, gid, targetID int64, muted bool) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}
	if !me.IsAdmin() {
		return resp.WithMsg(resp.ErrForbidden, "没有权限设置禁言")
	}
	target, err := s.deps.Groups.GetMember(ctx, gid, targetID)
	if err != nil {
		return err
	}
	if target == nil {
		return resp.WithMsg(resp.ErrNotFound, "该成员不在群里")
	}
	if target.Role == model.GroupRoleOwner {
		return resp.WithMsg(resp.ErrForbidden, "不能禁言群主")
	}
	if err := s.deps.Groups.UpdateMember(ctx, gid, targetID, map[string]any{"muted": muted}); err != nil {
		return err
	}
	s.notify(ctx, gid, "member_mute", map[string]any{
		"group_id": gid, "operator": uid, "target": targetID, "muted": muted,
	})
	return nil
}

// SetAdmin 设置或取消管理员，仅群主可操作。
func (s *GroupService) SetAdmin(ctx context.Context, uid, gid, targetID int64, admin bool) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}
	if me.Role != model.GroupRoleOwner {
		return resp.WithMsg(resp.ErrForbidden, "只有群主可以设置管理员")
	}
	if targetID == uid {
		return resp.WithMsg(resp.ErrInvalidParam, "群主无需设置自己")
	}
	target, err := s.deps.Groups.GetMember(ctx, gid, targetID)
	if err != nil {
		return err
	}
	if target == nil {
		return resp.WithMsg(resp.ErrNotFound, "该成员不在群里")
	}
	role := model.GroupRoleMember
	if admin {
		role = model.GroupRoleAdmin
	}
	if err := s.deps.Groups.UpdateMember(ctx, gid, targetID, map[string]any{"role": role}); err != nil {
		return err
	}
	s.notify(ctx, gid, "role_change", map[string]any{
		"group_id": gid, "operator": uid, "target": targetID, "role": role,
	})
	return nil
}

// TransferOwner 转让群主，事务内完成角色互换。
func (s *GroupService) TransferOwner(ctx context.Context, uid, gid, targetID int64) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}
	if me.Role != model.GroupRoleOwner {
		return resp.WithMsg(resp.ErrForbidden, "只有群主可以转让群")
	}
	if targetID == uid {
		return resp.WithMsg(resp.ErrInvalidParam, "不能转让给自己")
	}
	target, err := s.deps.Groups.GetMember(ctx, gid, targetID)
	if err != nil {
		return err
	}
	if target == nil {
		return resp.WithMsg(resp.ErrNotFound, "该成员不在群里")
	}

	err = s.deps.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Group{}).Where("id = ?", gid).
			Update("owner_id", targetID).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.GroupMember{}).
			Where("group_id = ? AND user_id = ?", gid, uid).
			Update("role", model.GroupRoleMember).Error; err != nil {
			return err
		}
		return tx.Model(&model.GroupMember{}).
			Where("group_id = ? AND user_id = ?", gid, targetID).
			Update("role", model.GroupRoleOwner).Error
	})
	if err != nil {
		return err
	}
	s.notify(ctx, gid, "owner_change", map[string]any{
		"group_id": gid, "operator": uid, "target": targetID,
	})
	return nil
}

// Dissolve 解散群，仅群主可操作。
func (s *GroupService) Dissolve(ctx context.Context, uid, gid int64) error {
	_, me, err := s.mustBeMember(ctx, uid, gid)
	if err != nil {
		return err
	}
	if me.Role != model.GroupRoleOwner {
		return resp.WithMsg(resp.ErrForbidden, "只有群主可以解散群")
	}
	if err := s.deps.Groups.UpdateFields(ctx, gid, map[string]any{"status": model.GroupStatusDissolved}); err != nil {
		return err
	}
	s.rel.InvalidateGroupMembers(ctx, gid)
	s.notify(ctx, gid, "dissolve", map[string]any{"group_id": gid, "operator": uid})
	return nil
}

// mustBeMember 校验群存在且调用者是成员，返回群与成员信息。
func (s *GroupService) mustBeMember(ctx context.Context, uid, gid int64) (*model.Group, *model.GroupMember, error) {
	g, err := s.deps.Groups.Get(ctx, gid)
	if err != nil {
		return nil, nil, err
	}
	if g == nil || g.Status != model.GroupStatusNormal {
		return nil, nil, resp.WithMsg(resp.ErrNotFound, "群不存在或已解散")
	}
	m, err := s.deps.Groups.GetMember(ctx, gid, uid)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, resp.WithMsg(resp.ErrForbidden, "你不在该群中")
	}
	return g, m, nil
}

// notify 向群成员广播群事件；后续可迁移到 MQ 做削峰。
func (s *GroupService) notify(ctx context.Context, gid int64, action string, data map[string]any) {
	ids, err := s.rel.MemberIDs(ctx, gid)
	if err != nil || len(ids) == 0 {
		return
	}
	data["action"] = action
	s.deps.Pusher.PushToUsers(ids, NewFrame(FrameGroupEvent, data).Encode())
}

func (s *GroupService) toItem(g *model.Group, role int8, muted bool) *GroupItem {
	return &GroupItem{
		ID: g.ID, Name: g.Name, Avatar: g.Avatar, OwnerID: g.OwnerID,
		Notice: g.Notice, MemberCount: g.MemberCount, MaxMember: g.MaxMember,
		MyRole: role, Muted: muted, CreatedAt: g.CreatedAt,
	}
}
