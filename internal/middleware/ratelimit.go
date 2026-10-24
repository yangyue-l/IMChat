package middleware

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"imchat/internal/cache"
	"imchat/internal/pkg/resp"
)

// Rule 是一条限流规则。判据只有一档，但有一档降级：
//
//	Global > 0 且后端可用  -> Redis 滑动窗口，跨实例共享总配额；
//	否则                   -> 进程内令牌桶，零网络开销。
//
// 为什么必须分开：令牌桶在集群下配额 = 单机配额 × 节点数，扩容等于放宽限流，
// 这是它唯一的硬伤；而滑动窗口每次都要一次 Redis 往返，给高频读接口加这笔
// 开销纯属浪费。所以读接口用令牌桶，登录、发消息这类要真正封顶的用滑动窗口。
type Rule struct {
	Name   string        // 规则名，同时作为计数命名空间，不同规则互不干扰
	QPS    float64       // 令牌桶速率，也是 Global 档在 Redis 故障时的降级水位；<=0 表示不限
	Burst  int           // 令牌桶突发容量
	Global int64         // >0：窗口内允许的次数，跨实例共享
	Window time.Duration // 滑动窗口长度，Global > 0 时必填
}

// GlobalStore 是分布式限流后端，由 cache.Cache 实现。
type GlobalStore interface {
	AllowSlide(ctx context.Context, key string, limit int64, window time.Duration) (cache.RateState, error)
}

// limiterEntry 保存单个键的令牌桶与最后访问时间。
type limiterEntry struct {
	lim  *rate.Limiter
	last time.Time
}

// decision 是一次限流判定结果。
type decision struct {
	ok         bool
	limit      int64         // 429 时回给客户端的额度上界
	retryAfter time.Duration // 429 时建议的重试等待
}

// Limiter 持有本进程的令牌桶集合，可选叠加 Redis 全局窗口。
type Limiter struct {
	enabled bool
	store   GlobalStore

	mu      sync.Mutex
	entries map[string]*limiterEntry
	once    sync.Once
}

// NewLimiter 创建限流器。store 为 nil 时只用本地令牌桶，等价于单机模式。
func NewLimiter(enabled bool, store GlobalStore) *Limiter {
	return &Limiter{
		enabled: enabled,
		store:   store,
		entries: make(map[string]*limiterEntry),
	}
}

// ByUID 按用户维度限流，未登录时退化为按 IP 限流。
// 已鉴权接口挂这个：令牌桶挂在 uid 上，换 IP 绕不过去。
func (l *Limiter) ByUID(rule Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.active(rule) {
			c.Next()
			return
		}
		uid := UID(c)
		key := "u" + strconv.FormatInt(uid, 10)
		if uid == 0 {
			// 走到这里说明鉴权没生效，用 IP 兜底，不能让匿名请求裸奔
			key = "ip" + c.ClientIP()
		}
		if !l.guard(c, rule, key) {
			return
		}
		c.Next()
	}
}

// ByIP 按 IP 维度限流，用于登录、注册、WebSocket 握手这些还没有身份的阶段。
func (l *Limiter) ByIP(rule Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.active(rule) {
			c.Next()
			return
		}
		if !l.guard(c, rule, "ip"+c.ClientIP()) {
			return
		}
		c.Next()
	}
}

func (l *Limiter) active(rule Rule) bool {
	return l.enabled && (rule.QPS > 0 || rule.Global > 0)
}

// guard 判定并写响应。返回 false 表示已中断请求，调用方不要再 Next。
func (l *Limiter) guard(c *gin.Context, rule Rule, subject string) bool {
	d := l.permit(c.Request.Context(), rule, subject)
	if d.ok {
		return true
	}

	// 只在拒绝时写限流头：正常请求也写会平白多几次字符串拼接，
	// 而客户端真正需要的是「被拒了、等多久」，X-RateLimit-Limit 只是辅助排查。
	secs := int64((d.retryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	h := c.Writer.Header()
	h.Set("Retry-After", strconv.FormatInt(secs, 10))
	h.Set("X-RateLimit-Limit", strconv.FormatInt(d.limit, 10))
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Rule", rule.Name)

	resp.Abort(c, resp.WithMsg(resp.ErrTooManyRequests, "请求过于频繁，请 "+strconv.FormatInt(secs, 10)+" 秒后重试"))
	return false
}

// permit 做一次判定：两档串联，先瞬时速率，再窗口总量。
//
// 本地令牌桶在前是因为它是最便宜的闸门：绝大多数超频流量在进程内就被挡住，
// 不必陪一次 Redis 往返 —— Redis 在主链路上，不该被垃圾请求消耗。
//
// 反过来的代价是「本地放行、Redis 拒绝」时会白扣一个令牌。走到这个分支
// 说明用户已经把整个窗口的额度用完，接下来一分钟内令牌本来就一直是满的，
// 所以白扣无害；反过来若把 Redis 放在前面，每次超频请求都要打一次 Redis，
// 那才是真代价。
//
// 两档组合的效果：令牌桶限制「此刻能不能打这么快」，滑动窗口限制
// 「这段时间总共能用多少」。单靠令牌桶的话，1 秒内打满 burst 后只要
// 保持低速率就能无限累计，总量没有上界。
func (l *Limiter) permit(ctx context.Context, rule Rule, subject string) decision {
	if d := l.local(rule, subject); !d.ok {
		return d
	}

	if rule.Global > 0 && l.store != nil {
		st, err := l.store.AllowSlide(ctx, rule.Name+":"+subject, rule.Global, rule.Window)
		if err != nil {
			// Redis 不可用：本地桶已经判过，直接放行。
			// 限流组件故障不该把登录和发消息一起掐死。
			return decision{ok: true}
		}
		if !st.Allowed {
			return decision{limit: rule.Global, retryAfter: st.RetryAfter}
		}
	}
	return decision{ok: true}
}

// local 进程内令牌桶判定。
func (l *Limiter) local(rule Rule, subject string) decision {
	if rule.QPS <= 0 {
		return decision{ok: true}
	}
	key := rule.Name + "|" + subject
	now := time.Now()

	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		// Burst 为 0 时 rate.Limiter 永远拿不到令牌，这里兜底成 1，避免配置漏填变成全拒
		burst := rule.Burst
		if burst <= 0 {
			burst = 1
		}
		e = &limiterEntry{lim: rate.NewLimiter(rate.Limit(rule.QPS), burst), last: now}
		l.entries[key] = e
	}
	e.last = now
	l.mu.Unlock()

	// Allow 本身并发安全，放在锁外调用，减少临界区
	if e.lim.Allow() {
		return decision{ok: true}
	}
	return decision{
		limit:      int64(e.lim.Burst()),
		retryAfter: time.Duration(float64(time.Second) / rule.QPS),
	}
}

// StartCleanup 定期清理长时间不活跃的桶，防止 map 随用户数无限增长。
// 超过 ttl 没有任何请求的键，令牌桶早已回满，删掉不影响后续判定。
func (l *Limiter) StartCleanup(interval, ttl time.Duration) {
	l.once.Do(func() {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				cutoff := time.Now().Add(-ttl)
				l.mu.Lock()
				for k, e := range l.entries {
					if e.last.Before(cutoff) {
						delete(l.entries, k)
					}
				}
				l.mu.Unlock()
			}
		}()
	})
}
