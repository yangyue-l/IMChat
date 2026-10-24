package handler

import (
	"github.com/gin-gonic/gin"

	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// listFriends 好友列表。
func (r *Router) listFriends(c *gin.Context) {
	list, err := r.svc.Friend.List(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// deleteFriend 删除好友。
func (r *Router) deleteFriend(c *gin.Context) {
	uid, err := pathID(c, "friend_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Friend.Delete(c.Request.Context(), middleware.UID(c), uid); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已删除好友"})
}

// updateFriend 修改备注或星标。
func (r *Router) updateFriend(c *gin.Context) {
	friendID, err := pathID(c, "friend_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req UpdateFriendReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Friend.Update(c.Request.Context(), middleware.UID(c), friendID, req.Remark, req.Starred); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已更新"})
}

// listFriendRequests 好友申请列表，type=incoming|outgoing。
func (r *Router) listFriendRequests(c *gin.Context) {
	incoming := c.DefaultQuery("type", "incoming") != "outgoing"
	list, err := r.svc.Friend.Requests(c.Request.Context(), middleware.UID(c), incoming)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// sendFriendRequest 发起好友申请。
func (r *Router) sendFriendRequest(c *gin.Context) {
	var req service.SendFriendReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Friend.SendRequest(c.Request.Context(), middleware.UID(c), req); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "申请已发送"})
}

// handleFriendRequest 处理好友申请。
func (r *Router) handleFriendRequest(c *gin.Context) {
	requestID, err := pathID(c, "request_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req HandleFriendRequestReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	// 必须显式判断：漏传 accept 时不能用零值 false 兜底，否则等于静默拒绝申请
	if req.Accept == nil {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "缺少 accept 参数"))
		return
	}
	if err := r.svc.Friend.HandleRequest(c.Request.Context(), middleware.UID(c), requestID, *req.Accept); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已处理"})
}

// listBlacklist 黑名单列表。
func (r *Router) listBlacklist(c *gin.Context) {
	list, err := r.svc.Friend.Blocked(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// blockUser 拉黑用户。
func (r *Router) blockUser(c *gin.Context) {
	var req UserTargetReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Friend.Block(c.Request.Context(), middleware.UID(c), req.UserID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已加入黑名单"})
}

// unblockUser 解除拉黑。
func (r *Router) unblockUser(c *gin.Context) {
	targetID, err := pathID(c, "user_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Friend.Unblock(c.Request.Context(), middleware.UID(c), targetID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已移出黑名单"})
}
