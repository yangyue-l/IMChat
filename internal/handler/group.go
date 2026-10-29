package handler

import (
	"github.com/gin-gonic/gin"

	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// createGroup 建群。
func (r *Router) createGroup(c *gin.Context) {
	var req service.CreateGroupReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	g, err := r.svc.Group.Create(c.Request.Context(), middleware.UID(c), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, g)
}

// listGroups 我加入的群。
func (r *Router) listGroups(c *gin.Context) {
	list, err := r.svc.Group.List(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// groupDetail 群详情与成员。
func (r *Router) groupDetail(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	d, err := r.svc.Group.Detail(c.Request.Context(), middleware.UID(c), gid)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, d)
}

// listGroupMembers 分页查看群成员。
func (r *Router) listGroupMembers(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	offset, limit, err := queryPaging(c)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	page, err := r.svc.Group.Members(c.Request.Context(), middleware.UID(c), gid, offset, limit)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, page)
}

// updateGroup 修改群资料。
func (r *Router) updateGroup(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req service.UpdateGroupReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.Update(c.Request.Context(), middleware.UID(c), gid, req); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已更新"})
}

// addGroupMembers 邀请成员。
func (r *Router) addGroupMembers(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req AddGroupMembersReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	added, err := r.svc.Group.AddMembers(c.Request.Context(), middleware.UID(c), gid, req.UserIDs)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"added": added})
}

// removeGroupMember 踢人或退群（user_id 为自己即退群）。
func (r *Router) removeGroupMember(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	target, err := pathID(c, "user_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.RemoveMember(c.Request.Context(), middleware.UID(c), gid, target); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已处理"})
}

// muteGroupMember 设置禁言。
func (r *Router) muteGroupMember(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req MuteMemberReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.MuteMember(c.Request.Context(), middleware.UID(c), gid, req.UserID, req.Muted); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已更新"})
}

// setGroupAdmin 设置或取消管理员。
func (r *Router) setGroupAdmin(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req SetGroupAdminReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.SetAdmin(c.Request.Context(), middleware.UID(c), gid, req.UserID, req.Admin); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已更新"})
}

// transferGroupOwner 转让群主。
func (r *Router) transferGroupOwner(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req UserTargetReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.TransferOwner(c.Request.Context(), middleware.UID(c), gid, req.UserID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已转让"})
}

// dissolveGroup 解散群。
func (r *Router) dissolveGroup(c *gin.Context) {
	gid, err := pathID(c, "group_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Group.Dissolve(c.Request.Context(), middleware.UID(c), gid); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "群已解散"})
}
