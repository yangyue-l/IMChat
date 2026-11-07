package handler

import (
	"github.com/gin-gonic/gin"

	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/service"
)

// register 注册账号并返回令牌。
func (r *Router) register(c *gin.Context) {
	var req service.RegisterReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	result, err := r.svc.User.Register(c.Request.Context(), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, result)
}

// login 账号密码登录。
func (r *Router) login(c *gin.Context) {
	var req service.LoginReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	result, err := r.svc.User.Login(c.Request.Context(), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, result)
}

// logout 登出并把令牌加入黑名单。
func (r *Router) logout(c *gin.Context) {
	token := middleware.Token(c)
	if err := r.svc.User.Logout(c.Request.Context(), token, r.jwt.Expire()); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"message": "已退出登录"})
}

// me 当前登录用户资料。
func (r *Router) me(c *gin.Context) {
	u, err := r.svc.User.Profile(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, u)
}

// updateProfile 修改资料。
func (r *Router) updateProfile(c *gin.Context) {
	var req service.UpdateProfileReq
	if err := bindJSON(c, &req); err != nil {
		resp.Fail(c, err)
		return
	}
	u, err := r.svc.User.UpdateProfile(c.Request.Context(), middleware.UID(c), req)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, u)
}

// searchUsers 搜索用户。
func (r *Router) searchUsers(c *gin.Context) {
	uid := middleware.UID(c)
	limit, err := queryNonNegInt(c, "limit")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	list, err := r.svc.User.Search(c.Request.Context(), uid, c.Query("keyword"), limit)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, list)
}

// getUser 查看其他用户资料。
func (r *Router) getUser(c *gin.Context) {
	id, err := pathID(c, "user_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	u, err := r.svc.User.Profile(c.Request.Context(), id)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, u)
}

// userOnline 查询某用户是否在线。只能查自己或好友，判定逻辑与集群差异都在 service。
func (r *Router) userOnline(c *gin.Context) {
	id, err := pathID(c, "user_id")
	if err != nil {
		resp.Fail(c, err)
		return
	}
	online, err := r.svc.User.Online(c.Request.Context(), middleware.UID(c), id)
	if err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"user_id": id, "online": online})
}

// listSessions 列出当前账号在线的设备。
//
// current 用令牌里的 device 标注而不写进 service：设备列表是「这个账号有哪些端」，
// 「哪一个是本机」取决于这次请求用的是哪个令牌，属于传输层语义。
// 客户端据此把本机排在第一行、并隐掉它的踢线按钮。
func (r *Router) listSessions(c *gin.Context) {
	devices, err := r.svc.User.Sessions(c.Request.Context(), middleware.UID(c))
	if err != nil {
		resp.Fail(c, err)
		return
	}
	current := middleware.Device(c)
	items := make([]gin.H, 0, len(devices))
	for _, device := range devices {
		items = append(items, gin.H{"device": device, "current": device == current})
	}
	resp.OK(c, gin.H{"devices": items})
}

// kickDevice 踢掉指定设备的登录。允许踢当前设备，客户端收到 4001 后重连。
func (r *Router) kickDevice(c *gin.Context) {
	device := c.Param("device")
	if err := r.svc.User.KickDevice(c.Request.Context(), middleware.UID(c), device); err != nil {
		resp.Fail(c, err)
		return
	}
	resp.OK(c, gin.H{"device": device})
}
