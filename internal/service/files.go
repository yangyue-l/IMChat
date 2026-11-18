package service

import (
	"context"
	"log/slog"
	"time"

	"imchat/internal/media"
)

// objectDropTimeout 是回收对象的独立超时。
//
// 回收发生在请求收尾，不能跟着接口的超时走：主链路已经超时了，这里更不该
// 再等满 OSS 客户端的读写超时（60s），把一次响应拖成几十秒。
const objectDropTimeout = 5 * time.Second

const (
	// maxAvatarBytes 与 users.avatar / groups.avatar 的列宽一致（varchar(255)）。
	// 超长在 MySQL 严格模式下是 1406 错误，抛到 handler 就成了 500，
	// 前端拿不到「你传的地址太长」这种可行动的信息。
	maxAvatarBytes = 255

	// maxImageURLBytes 是单张图片地址的长度上限。moments.images 是 text 列，
	// 本身不设限，但一条动态最多 9 张，不设上限等于允许用一条动态塞进整表大小的文本。
	maxImageURLBytes = 512
)

// dropObjects 尽力回收本次操作后不再被引用的对象。
//
// 四条约束，每条都对应一种真实故障：
//   - 只在业务写成功之后调用。先删对象再写库，写库失败就留下一条指向空对象的
//     记录，用户看到的是裂图；反过来最坏只留一个孤儿，不影响任何读取。
//   - owner 传「这个对象的归属者」而不是「当前操作者」：管理员撤回他人消息时
//     两者不同，用操作者判断会把本该回收的对象全部跳过。
//   - 用脱离请求的 context。用户传完图立刻关页面时请求 context 已经取消，
//     直接拿它去删会让清理静默跳过，孤儿只增不减。
//   - 删除失败只记日志：回收是附带动作，不能把一次成功的业务操作变成失败。
func dropObjects(ctx context.Context, files Files, owner int64, raws ...string) {
	if files == nil || len(raws) == 0 {
		return
	}
	keys := make([]string, 0, len(raws))
	for _, raw := range raws {
		// 占位符（avatar://A）、外部图、以及别人上传的对象一律跳过：
		// 这些值来自用户可写的字段，照单全收就会删掉别人的图片。
		if !media.Owned(raw, owner) {
			continue
		}
		keys = append(keys, media.Key(raw))
	}
	if len(keys) == 0 {
		return
	}
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), objectDropTimeout)
	defer cancel()
	if err := files.Delete(dropCtx, keys...); err != nil {
		// 孤儿对象不影响功能，但会一直占存储，必须留痕以便按日志补删
		slog.Warn("回收对象失败", "keys", keys, "err", err)
	}
}
