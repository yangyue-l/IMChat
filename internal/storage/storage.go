// Package storage 是文件存储的唯一入口，当前落到阿里云 OSS。
//
// handler 只依赖 Storage 接口，不认识 bucket、endpoint 与签名细节：
// 换 CDN、换区域、以后再加别的对象存储，都只改本包与配置，不动业务流程。
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrUnavailable 表示存储后端不可用：桶不可达、凭据失效、网络超时。
// 调用方只需要知道「没写成」，具体原因在错误链里。
var ErrUnavailable = errors.New("storage unavailable")

// Object 是一次写入。
//
// Key 由 media.NewKey 生成，是 '/' 分隔的相对路径，既不带前导斜杠也不带域名。
// 本包不解释 key 的目录结构，只负责把字节写到这个名字下面。
type Object struct {
	Key         string
	Body        io.Reader
	Size        int64
	ContentType string
}

// Storage 是文件存储的唯一入口。
//
// 只开写入不提供读取：文件名随机、内容本就要公开，读一律走对象存储的
// 公网地址或 CDN，应用层不转发图片流量。
type Storage interface {
	// Put 写入对象并返回可直接交给前端的 URL。
	Put(ctx context.Context, obj Object) (url string, err error)
	// URL 把 key 还原成访问地址，供按 key 存库或存量数据转换的场景使用。
	URL(key string) string
	// Delete 批量删除对象，用于换头像、删动态、撤回图片后的空间回收。
	//
	// 实现必须幂等：对象已经不存在时返回 nil。调用方拿到的是「本来就该没了」
	// 的旧地址，为此写分支判断不存在的错误没有任何收益。
	Delete(ctx context.Context, keys ...string) error
	// Ping 探活，供启动自检与 /healthz 复用。
	Ping(ctx context.Context) error
}

// Config 是构造 Storage 的参数，由 upload.oss 配置段映射而来。
//
// 不含对外访问前缀：那是 media 包的事（入库存 key、出站拼地址），
// 存储层只管往桶里写，不该知道 CDN 域名长什么样。
type Config struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	AccessKeySecret string
}
