package handler

import (
	"github.com/gin-gonic/gin"

	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// listConversations 会话列表。
func (r *Router) listConversations(c *gin.Context) {
	list, err := r.svc.Message.Conversations(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// updateConversation 设置置顶或免打扰。
func (r *Router) updateConversation(c *gin.Context) {
	convID := c.Param("conv_id")
	var req UpdateConversationReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Message.SetConversationFlag(c.Request.Context(), middleware.UID(c), convID, req.Pinned, req.Muted); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已更新"})
}

// history 拉取历史消息，游标为 before_seq。
func (r *Router) history(c *gin.Context) {
	convID := c.Query("conv_id")
	if convID == "" {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "缺少 conv_id"))
		return
	}
	list, err := r.svc.Message.History(c.Request.Context(), middleware.UID(c), convID,
		queryInt64(c, "before_seq", 0), queryInt(c, "limit", 20))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// sendPrivate 发送私聊消息（HTTP 通道，WS 通道同样可用）。
func (r *Router) sendPrivate(c *gin.Context) {
	var req service.SendReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	req.ConvType = 1
	dto, err := r.svc.Message.SendPrivate(c.Request.Context(), middleware.UID(c), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, dto)
}

// sendGroup 发送群消息。
func (r *Router) sendGroup(c *gin.Context) {
	var req service.SendReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	req.ConvType = 2
	dto, err := r.svc.Message.SendGroup(c.Request.Context(), middleware.UID(c), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, dto)
}

// markRead 上报已读位置。
func (r *Router) markRead(c *gin.Context) {
	var req MarkReadReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Message.MarkRead(c.Request.Context(), middleware.UID(c), req.ConvID, req.Seq); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "ok"})
}

// recall 撤回消息。
func (r *Router) recall(c *gin.Context) {
	msgID, err := pathID(c, "message_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Message.Recall(c.Request.Context(), middleware.UID(c), msgID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已撤回"})
}
