package handler

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"imchat/internal/media"
	"imchat/internal/middleware"
	"imchat/internal/pkg/resp"
	"imchat/internal/storage"
)

// multipartOverhead 是 multipart 报文头与分隔符的预留字节。
//
// MaxBytesReader 限制的是整个请求体，而 body 里除了文件字节还有 boundary、
// Content-Disposition 这些元数据。上限直接取文件上限的话，一个正好 10MB 的
// 文件会因为多出几百字节被拒，而报错还是「请选择要上传的文件」。
const multipartOverhead = 1 << 20

// allowedImage 是允许上传的图片白名单，值即落存储时使用的 Content-Type。
//
// 一张表同时承担两件事：挡掉可执行文件，以及给出准确的 MIME。
// 拆成两张表迟早会漂移——白名单放行了 .webp，MIME 表却漏了它，
// 落到对象存储后就变成 application/octet-stream，浏览器按下载处理。
var allowedImage = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp",
}

// upload 上传图片（头像、聊天图片、朋友圈配图共用）。
//
// 只做参数校验，字节交给 storage 写：本函数不认识 bucket、endpoint 与签名，
// 换区域、换桶、将来换别家对象存储都不需要改这里。
func (r *Router) upload(c *gin.Context) {
	// 连接级 deadline 与 ctx 超时是两套机制：ctx 只取消业务逻辑，
	// 而 http.Server 的 ReadTimeout（默认 15s）到时会把连接直接关掉，
	// 后端连一句「上传超时」都发不出去。10MB 在弱网上 15s 传不完，
	// 所以按 upload.timeout 重新设一次连接 deadline。
	// ResponseWriter 不支持时返回 ErrNotSupported（如测试用的 httptest），
	// 此时靠上面的 ctx 超时兜底，不影响功能。
	rc := http.NewResponseController(c.Writer)
	deadline := time.Now().Add(r.cfg.Upload.Timeout.Get())
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)

	limit := int64(r.cfg.Upload.MaxSizeMB) << 20
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+multipartOverhead)

	file, err := c.FormFile("file")
	if err != nil {
		// 请求体超过 MaxBytesReader 的上限时 FormFile 回的是 MaxBytesError，
		// 混进「请选择要上传的文件」会让人完全找不到原因。
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, fmt.Sprintf("文件不能超过 %dMB", r.cfg.Upload.MaxSizeMB)))
			return
		}
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "请选择要上传的文件"))
		return
	}
	ext := strings.ToLower(filepath.Ext(file.Filename))
	contentType, ok := allowedImage[ext]
	if !ok {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "仅支持 jpg/png/gif/webp 格式"))
		return
	}
	if file.Size > limit {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, fmt.Sprintf("文件不能超过 %dMB", r.cfg.Upload.MaxSizeMB)))
		return
	}
	// 0 字节的图谁都打不开，挡在这里比落进存储再变成坏数据便宜
	if file.Size <= 0 {
		resp.Fail(c, resp.WithMsg(resp.ErrInvalidParam, "文件内容为空"))
		return
	}

	// 用流式读取而不是 c.SaveUploadedFile：后者只会写本地磁盘，
	// 而图片要落到对象存储，中间必须有一段「字节交给 storage」的接口。
	src, err := file.Open()
	if err != nil {
		r.log.Error("打开上传文件失败", "err", err)
		resp.Fail(c, resp.WithMsg(resp.ErrInternal, "文件读取失败"))
		return
	}
	defer func() { _ = src.Close() }()

	// key 里带上上传者 uid：回收时靠它判断对象归属，见 media.Owned
	key := media.NewKey(time.Now(), middleware.UID(c), ext)

	url, err := r.files.Put(c.Request.Context(), storage.Object{
		Key:         key,
		Body:        src,
		Size:        file.Size,
		ContentType: contentType,
	})
	if err != nil {
		// 存储侧的失败原因必须进日志：接口只回「保存失败」，
		// 没有这条日志就分不清是磁盘满了、桶配错了还是凭据过期。
		r.uploadFailed.Add(1)
		r.log.Error("图片上传失败", "ext", ext, "size", file.Size, "err", err)
		resp.Fail(c, resp.WithMsg(resp.ErrInternal, "文件保存失败"))
		return
	}
	r.uploadOK.Add(1)
	r.uploadBytes.Add(file.Size)

	// 同时回 key 与 url：url 给前端直接渲染，key 是入库形态。
	// 只回 url 的话，前端存进 avatar / images 的就是绝对地址，换 CDN 要回刷数据。
	resp.OK(c, gin.H{"key": key, "url": url, "size": file.Size})
}
