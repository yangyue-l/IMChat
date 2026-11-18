package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	aliyun "github.com/aliyun/aliyun-oss-go-sdk/oss"

	"imchat/internal/media"
)

const (
	// SDK 的超时是客户端级配置（单位秒），不接 context：
	// 连接 10s 挡住 DNS 与握手卡死，读写 60s 给弱网手机留余量
	// （单文件上限 10MB，正常网络远用不满）。
	connectTimeoutSec   = 10
	readWriteTimeoutSec = 60

	// probeKey 用于启动与 /healthz 探活，属于绝不会被业务写入的前缀。
	probeKey = "_probe/health"

	// cacheControlImmutable 让 CDN 与浏览器长期缓存图片。
	//
	// key 里带雪花 ID，同一个 key 的内容永不变，长缓存不会导致「改了图看不到」；
	// 不显式声明时按桶/ CDN 的默认策略走，很可能每次都要回源，
	// 图片流量全部压在桶上。
	cacheControlImmutable = "public, max-age=31536000, immutable"

	// probeTTL 是探活结果的缓存时长。
	//
	// /healthz 由负载均衡每隔几秒拉一次，实例一多，真打桶的请求量就随实例数
	// 线性放大，而每次 HEAD 都是一个 RTT 加一次签名。缓存几秒能把探活流量
	// 压成常数级，同时把「桶真的挂了」的发现延迟限制在 probeTTL 以内——
	// 探活要的是「尽快发现」，不是「每一次都问一遍」。
	probeTTL = 10 * time.Second
)

// ossStorage 是阿里云 OSS 实现。
type ossStorage struct {
	bucket *aliyun.Bucket
	// objectACL 为空表示不显式设置，对象 ACL 跟随桶的默认策略。
	// 桶是私有时必须设 public-read，否则图片地址对外一律 403。
	objectACL aliyun.ACLType
	// probeFn 是真打桶的探测。字段化是为了让「ctx 先到」「缓存命中」
	// 这几条分支能在不联网、不持有 OSS 凭据的前提下测出来。
	probeFn func() error

	// 探活缓存，见 probeTTL。
	mu       sync.Mutex
	probedAt time.Time
	probeErr error
	// inflight 非 nil 表示有探测正在跑，close 即完成。
	// 并发探活只需要等同一个结果，不该让 N 个 /healthz 打出 N 次 HEAD。
	inflight chan struct{}
}

// New 构造 OSS 存储。
//
// 这里不发网络请求：SDK 的 Client 与 Bucket 都是本地构造，配置错、凭据错
// 要到第一次 Put 或显式 Ping 才暴露，所以启动时必须探一次活。
func New(cfg Config) (Storage, error) {
	switch {
	case cfg.Endpoint == "" || cfg.Bucket == "":
		return nil, fmt.Errorf("storage: oss.endpoint 与 oss.bucket 不能为空")
	case cfg.AccessKeyID == "" || cfg.AccessKeySecret == "":
		return nil, fmt.Errorf("storage: oss 凭据未配置（IMCHAT_OSS_ACCESS_KEY_ID / IMCHAT_OSS_ACCESS_KEY_SECRET）")
	}

	// endpoint 允许不带协议头，缺省用 https：明文链路上凭据与图片都可能被替换
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	client, err := aliyun.New(endpoint, cfg.AccessKeyID, cfg.AccessKeySecret,
		aliyun.Timeout(connectTimeoutSec, readWriteTimeoutSec))
	if err != nil {
		return nil, fmt.Errorf("storage: 初始化 OSS 客户端失败: %w", err)
	}
	bucket, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: 打开 bucket %s 失败: %w", cfg.Bucket, err)
	}
	s := &ossStorage{bucket: bucket, objectACL: aliyun.ACLType(cfg.ObjectACL)}
	return s, nil
}

// Put 上传对象，成功后返回公网地址。
func (s *ossStorage) Put(ctx context.Context, obj Object) (string, error) {
	// 客户端已经断开就不必再往 OSS 传
	if err := ctx.Err(); err != nil {
		return "", err
	}
	opts := []aliyun.Option{
		aliyun.ContentType(obj.ContentType),
		// 显式声明长度，让 OSS 校验收全了没有：只靠分块传输时，
		// 中途断开也可能留下一个「接口成功但内容截断」的对象。
		aliyun.ContentLength(obj.Size),
		// 同名对象直接失败而不是覆盖：key 是雪花 ID，真撞上说明 ID 生成出了问题，
		// 静默覆盖会让前一个人的图片变成后一个人的。
		aliyun.ForbidOverWrite(true),
		// key 内容永不变，长缓存是纯收益，见 cacheControlImmutable。
		aliyun.CacheControl(cacheControlImmutable),
	}
	// 留空就不带这个头，让对象继承桶策略：桶本身已开放读时，
	// 再显式设一遍只是把同一个结论写两处，反而多一个会漂移的地方。
	if s.objectACL != "" {
		opts = append(opts, aliyun.ObjectACL(s.objectACL))
	}
	// SDK 不接 context，但它会持续读 body：包一层之后，取消后的下一次 Read
	// 立刻返回错误，上传随之中断，不必等满 SDK 的 60s 读写超时。
	body := &ctxReader{ctx: ctx, r: obj.Body}
	if err := s.bucket.PutObject(obj.Key, body, opts...); err != nil {
		return "", fmt.Errorf("%w: 上传 %s 失败: %w", ErrUnavailable, obj.Key, err)
	}
	return s.URL(obj.Key), nil
}

// URL 把 key 拼成公网地址。
//
// 拼接规则统一在 media 里实现：库里存的也是 key，两边必须是同一套规则，
// 否则会出现「写进去的地址和读出来的地址不一样」这种最难查的错位。
func (s *ossStorage) URL(key string) string {
	return media.URL(key)
}

// Delete 批量删除对象。
//
// 对象不存在不算失败：OSS 的批量删除对不存在的 key 同样返回成功，这样
// 「重复删除」与「库里残留的历史地址」都不会把清理流程卡住。
func (s *ossStorage) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	// 客户端已经断开就不必再打一次网络
	if err := ctx.Err(); err != nil {
		return err
	}
	// OSS 单次批量删除上限是 1000 个 key；调用方一次最多回收 9 张图，
	// 这里不做分批，真需要时再按 1000 切块。
	if _, err := s.bucket.DeleteObjects(keys); err != nil {
		return fmt.Errorf("%w: 删除 %d 个对象失败: %w", ErrUnavailable, len(keys), err)
	}
	return nil
}

// Ping 探活。
//
// 用 HEAD 一个固定不存在的对象，而不是 GetBucketInfo：只授予
// oss:PutObject / oss:GetObject 的最小权限策略里通常没有 oss:GetBucketInfo，
// 拿后者探活会把一个健康的部署判成不可用。
//
// 调用方只等到自己的 deadline，不跟着 SDK 的「10s 连接 + 60s 读写」一起卡：
// 探测本身跑在独立 goroutine 里，结果回填缓存供后续调用复用。
func (s *ossStorage) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	if time.Since(s.probedAt) < probeTTL {
		err := s.probeErr
		s.mu.Unlock()
		return err
	}
	wait := s.inflight
	if wait == nil {
		wait = make(chan struct{})
		s.inflight = wait
		go s.runProbe(wait)
	}
	s.mu.Unlock()

	select {
	case <-wait:
		s.mu.Lock()
		err := s.probeErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		// 探测还在飞，结果留给下一个调用方：这里放弃不影响缓存回填。
		return fmt.Errorf("oss 探活超时: %w", ctx.Err())
	}
}

// runProbe 执行一次真实探测，完成后回填缓存并唤醒等待者。
func (s *ossStorage) runProbe(done chan struct{}) {
	probe := s.probeFn
	if probe == nil {
		probe = s.probe
	}
	err := probe()

	s.mu.Lock()
	s.probedAt = time.Now()
	s.probeErr = err
	s.inflight = nil
	s.mu.Unlock()

	close(done)
}

// probe 是真打一次桶的探测，失败原因只进错误链，由调用方决定怎么记。
func (s *ossStorage) probe() error {
	// false + nil 才算健康：桶可达、凭据有效、对象确实不存在
	exists, err := s.bucket.IsObjectExist(probeKey)
	if err != nil {
		return fmt.Errorf("oss 探活失败: %w", err)
	}
	if exists {
		// 这个 key 被业务写过，说明探活结论不可信，按异常处理
		return fmt.Errorf("oss 探活失败: 探测对象 %s 意外存在", probeKey)
	}
	return nil
}

// ctxReader 让不接 context 的 SDK 也能感知取消。
//
// 每次 Read 前查一次 ctx：ctx.Err() 是一次原子读，开销可以忽略，
// 而换成「只在边界检查」要额外维护状态，还容易漏掉真正卡住的那次读。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
