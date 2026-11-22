// Package media 统一「对象 key」与「对外访问地址」之间的互转。
//
// 库里只存 key（形如 u/1001/2026/11/18/1234567890.webp），出站时才拼成绝对地址：
// 换 CDN、换域名只改 upload.public_base 这一个配置，不必回刷 users.avatar、
// moments.images、messages.content 这些列。入站方向做反向还原，前端因此完全
// 不需要知道库里存的是哪一种。
//
// 单独成包而不是并进 storage：拼地址是纯字符串运算，model 的 DTO 转换也要用，
// 而 model 不该为了拼一个 URL 就依赖 OSS SDK。
//
// 放在 pkg 下而不是与 cache / storage 并列：这里没有外部依赖，也没有连接与生命周期，
// 只有一组纯函数加一个装配期注入的前缀，与 jsonx、snowflake 同类。
// cache / storage / mq / infra 那一层是外部服务适配，会建连、要探活，和它不是一个性质。
package media

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"imchat/internal/pkg/snowflake"
)

// ownerDir 是 key 的第一级目录，存放上传者 uid。
const ownerDir = "u"

// base 是图片对外的访问前缀（upload.public_base），由装配处调用 Init 写入。
//
// 用包级变量而不是到处传参：拼地址的调用点分散在 model 的 DTO 转换与 service 里，
// 把一个「初始化一次、之后只读」的字符串穿过十几层函数，收益不如对齐
// snowflake.Init 的既有做法。
var base string

// Init 记录对外访问前缀，由 cmd/server 在装配阶段调用一次。
//
// 入参已由 config 校验过是 http(s):// 开头的绝对地址，这里只做尾斜杠归一化：
// 前缀带不带尾斜杠是配置习惯问题，拼出 https://cdn//2026 就是 bug。
func Init(publicBase string) {
	base = strings.TrimRight(strings.TrimSpace(publicBase), "/")
}

// URL 把库里存的值还原成可直接访问的地址。
//
// 三种输入都必须正确，缺一种都会在真实数据上出问题：
//   - key（本次改动后写入的新数据）：拼上访问前缀；
//   - 绝对地址（历史数据、用户自己填的外部图）：原样返回，绝不能拼前缀；
//   - 其它 scheme 的占位符（默认头像 avatar://A）：同样原样返回。
func URL(raw string) string {
	if raw == "" || hasScheme(raw) || base == "" {
		return raw
	}
	return base + "/" + strings.TrimPrefix(raw, "/")
}

// URLs 批量转换。
//
// 保持 nil 与空切片的区别：动态的 images 为 nil 时前端拿到 null，
// 与改动前完全一致，不因为多了一层转换就改变响应形状。
func URLs(raw []string) []string {
	if raw == nil {
		return nil
	}
	out := make([]string, len(raw))
	for i := range raw {

		out[i] = URL(raw[i])
	}
	return out
}

// Key 把入参还原成对象 key。
//
// 前端上传后拿到的是绝对地址，回传时也照原样塞回来，所以写入前必须过这一道：
// 只有库里存 key，换 CDN 才不需要回刷数据。不是本服务发出的地址（外部图、
// avatar:// 占位符）原样保留 —— 出站时 URL 会把它原样返回，语义闭合。
func Key(raw string) string {
	if raw == "" {
		return ""
	}
	if !hasScheme(raw) {
		return strings.TrimPrefix(raw, "/")
	}
	if base == "" || !strings.HasPrefix(raw, base+"/") {
		return raw
	}
	return strings.TrimPrefix(raw, base+"/")
}

// NewKey 生成对象 key：u/{uid}/{yyyy}/{mm}/{dd}/{雪花}{ext}。
//
// 按日期分目录是为了单目录不堆过多对象；文件名用雪花 ID 丢掉用户原始文件名，
// 既挡路径穿越，也避免同名覆盖。带上传者 uid 不是为了展示，而是让「删除前
// 确认这个对象是当前用户传的」退化成一次前缀判断，见 Owned。
//
// 拼路径用 path.Join 而不是 filepath.Join：后者在 Windows 上产出反斜杠，
// 本地看不出差别，而对象存储会把整条带反斜杠的路径当成一个扁平的对象名。
func NewKey(now time.Time, ownerID int64, ext string) string {
	return path.Join(ownerDir, strconv.FormatInt(ownerID, 10),
		now.Format("2006/01/02"), fmt.Sprintf("%d%s", snowflake.Next(), ext))
}

// Owner 从 key 里解析上传者 uid。
// 不是本服务生成的 key（历史数据、外部地址、占位符）返回 false。
func Owner(key string) (int64, bool) {
	rest, ok := strings.CutPrefix(Key(key), ownerDir+"/")
	if !ok {
		return 0, false
	}
	idPart, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, false
	}
	uid, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || uid <= 0 {
		return 0, false
	}
	return uid, true
}

// Owned 判断 raw 指向的对象是否由 uid 上传。
//
// 回收对象前必须过这一关：avatar 这类字段允许用户填任意字符串，把别人的图片
// 地址填进自己的头像后，若照库里存的值直接删，删掉的就是别人的对象。
func Owned(raw string, uid int64) bool {
	owner, ok := Owner(raw)
	return ok && owner == uid
}

// hasScheme 判断字符串是否带 scheme（http://、https://、avatar://）。
//
// 规则与浏览器一致：字母开头，后接字母/数字/+/-/.，直到冒号。只认 "://" 会漏掉
// avatar:x 这类写法，宁可判宽一点：判宽的代价是少删一个对象，判窄的代价是把
// 访问前缀拼到一个本来就带 scheme 的值前面。
func hasScheme(raw string) bool {
	if raw == "" || !isAlpha(raw[0]) {
		return false
	}
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if c == ':' {
			return true
		}
		if isAlpha(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return false
}

func isAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
