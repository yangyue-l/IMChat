package handler

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/pkg/resp"
	"imchat/internal/pkg/snowflake"
)

// 允许上传的图片类型。只校验扩展名白名单，避免可执行文件被上传。
var allowedImageExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
}

// upload 上传图片（头像、聊天图片、朋友圈配图共用）。
func (r *Router) upload(c *gin.Context) {
	limit := int64(r.cfg.Upload.MaxSizeMB) << 20
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)

	file, err := c.FormFile("file")
	if err != nil {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "请选择要上传的文件"))
		return
	}
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if !allowedImageExt[ext] {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "仅支持 jpg/png/gif/webp 格式"))
		return
	}
	if file.Size > limit {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, fmt.Sprintf("文件不能超过 %dMB", r.cfg.Upload.MaxSizeMB)))
		return
	}

	// 按日期分目录，避免单目录文件过多
	subDir := time.Now().Format("2006/01/02")
	dir := filepath.Join(r.cfg.Upload.Dir, subDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		resp.Fail(c, resp.ErrInternal)
		return
	}

	// 用雪花 ID 重命名，避免用户原始文件名带来的路径穿越与重名覆盖
	name := fmt.Sprintf("%d%s", snowflake.Next(), ext)
	dst := filepath.Join(dir, name)
	if err := c.SaveUploadedFile(file, dst); err != nil {
		resp.Fail(c, resp.WithMsg(resp.ErrInternal, "文件保存失败"))
		return
	}

	url := strings.TrimRight(r.cfg.Upload.PublicBase, "/") + "/" + subDir + "/" + name
	resp.OK(c, gin.H{"url": url, "size": file.Size})
}
