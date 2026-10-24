package handler

import (
	"github.com/gin-gonic/gin"

	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// publishMoment 发布动态。
func (r *Router) publishMoment(c *gin.Context) {
	var req service.PublishMomentReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	dto, err := r.svc.Moment.Publish(c.Request.Context(), middleware.UID(c), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, dto)
}

// momentTimeline 好友动态流。
func (r *Router) momentTimeline(c *gin.Context) {
	list, err := r.svc.Moment.Timeline(c.Request.Context(), middleware.UID(c),
		queryInt64(c, "before_id", 0), queryInt(c, "limit", 10))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// userMoments 查看某人动态。
func (r *Router) userMoments(c *gin.Context) {
	authorID, err := pathID(c, "user_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	list, err := r.svc.Moment.UserMoments(c.Request.Context(), middleware.UID(c), authorID,
		queryInt64(c, "before_id", 0), queryInt(c, "limit", 10))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// deleteMoment 删除动态。
func (r *Router) deleteMoment(c *gin.Context) {
	momentID, err := pathID(c, "moment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Moment.Delete(c.Request.Context(), middleware.UID(c), momentID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已删除"})
}

// likeMoment 点赞。
func (r *Router) likeMoment(c *gin.Context) {
	momentID, err := pathID(c, "moment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Moment.Like(c.Request.Context(), middleware.UID(c), momentID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "ok"})
}

// unlikeMoment 取消点赞。
func (r *Router) unlikeMoment(c *gin.Context) {
	momentID, err := pathID(c, "moment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Moment.Unlike(c.Request.Context(), middleware.UID(c), momentID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "ok"})
}

// addComment 发表评论。
func (r *Router) addComment(c *gin.Context) {
	momentID, err := pathID(c, "moment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	var req AddCommentReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	dto, err := r.svc.Moment.Comment(c.Request.Context(), middleware.UID(c), momentID, req.Content, req.ReplyTo, req.ParentID)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, dto)
}

// deleteComment 删除评论。
func (r *Router) deleteComment(c *gin.Context) {
	commentID, err := pathID(c, "comment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	if err := r.svc.Moment.DeleteComment(c.Request.Context(), middleware.UID(c), commentID); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已删除"})
}

// listComments 评论列表。当前动态详情随列表返回，此接口用于单独刷新评论。
func (r *Router) listComments(c *gin.Context) {
	momentID, err := pathID(c, "moment_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	// 复用个人主页接口做权限校验：先确认动态可见，再返回动态本身携带的评论
	list, err := r.svc.Moment.MomentDetail(c.Request.Context(), middleware.UID(c), momentID)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list.Comments)
}
