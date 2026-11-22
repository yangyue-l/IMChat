// Package config 负责加载 YAML 配置并支持环境变量覆盖。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 yaml 支持 "30s" / "1h" 这类时长写法。
type Duration time.Duration

// maxUploadSizeMB 是单文件上限的封顶值。图片场景没有上百 MB 的正当需求，
// 而单请求的大小直接等于一次要落盘的字节数，放任配置等于放任磁盘被打满。
const maxUploadSizeMB = 100

// maxConsumerWorkers 是消费并发度上限：每个 worker 一个独立 channel，
// 超过这个量级基本是配置写错了，而不是真的需要这么多消费者。
const maxConsumerWorkers = 32

// UnmarshalYAML 实现 yaml 自定义解析。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("配置时长 %q 非法: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Get 返回标准库时长。
func (d Duration) Get() time.Duration { return time.Duration(d) }

type ServerConfig struct {
	Addr            string   `yaml:"addr"`
	Mode            string   `yaml:"mode"`
	ReadTimeout     Duration `yaml:"read_timeout"`
	WriteTimeout    Duration `yaml:"write_timeout"`
	WSPingInterval  Duration `yaml:"ws_ping_interval"`
	WSPongWait      Duration `yaml:"ws_pong_wait"`
	WSWriteWait     Duration `yaml:"ws_write_wait"`
	WSSendBuffer    int      `yaml:"ws_send_buffer"`
	GracefulTimeout Duration `yaml:"graceful_timeout"`
	Pprof           bool     `yaml:"pprof"`
	// Metrics 控制是否暴露 /metrics。用指针而不是 bool：要区分「没写」与
	// 「显式写 false」，而 bool 的零值和「未配置」长得一样。
	Metrics *bool `yaml:"metrics"`
	// Healthz 控制是否挂载 /healthz，语义与 Metrics 完全一致。
	//
	// 关掉只是不注册这条路由（请求落到 NoRoute 的 404），探活代码本身不会常驻：
	// 它没有后台协程、没有定时器，只在请求到达时才执行一次探测。
	// 但容器探针会因此拿到 404 而恒定判为不健康，Dockerfile 的 HEALTHCHECK
	// 与负载均衡的健康检查必须同步停掉，所以默认开启。
	Healthz *bool `yaml:"healthz"`
}

// MetricsEnabled 报告是否暴露 /metrics。
//
// 未配置视为开启 —— 监控是默认能力，关掉必须是显式决定。否则老配置升级后
// 会因为缺这个字段静默失去全部指标，那比这里多一个特例严重得多。
func (s ServerConfig) MetricsEnabled() bool {
	return s.Metrics == nil || *s.Metrics
}

// HealthzEnabled 报告是否挂载 /healthz。
//
// 未配置同样视为开启：探活是部署前提，老配置升级后若静默丢掉这个端点，
// 编排会拿到 404 把健康实例整批摘掉，而现象只是「服务不可用」，很难联想到配置。
// 本机不想留这条路由时显式写 healthz: false。
func (s ServerConfig) HealthzEnabled() bool {
	return s.Healthz == nil || *s.Healthz
}

type MySQLConfig struct {
	DSN             string   `yaml:"dsn"`
	MaxIdleConns    int      `yaml:"max_idle_conns"`
	MaxOpenConns    int      `yaml:"max_open_conns"`
	ConnMaxLifetime Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime Duration `yaml:"conn_max_idle_time"`
	AutoMigrate     bool     `yaml:"auto_migrate"`
}

type RedisConfig struct {
	Addr         string   `yaml:"addr"`
	Password     string   `yaml:"password"`
	DB           int      `yaml:"db"`
	PoolSize     int      `yaml:"pool_size"`
	MinIdleConns int      `yaml:"min_idle_conns"`
	DialTimeout  Duration `yaml:"dial_timeout"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
}

type RabbitMQConfig struct {
	URL              string `yaml:"url"`
	Enabled          bool   `yaml:"enabled"`
	Prefetch         int    `yaml:"prefetch"`
	PublisherConfirm bool   `yaml:"publisher_confirm"`
	ChannelPool      int    `yaml:"channel_pool"`
	// ConsumerWorkers 是消费并发度，每个 worker 独占一个 channel。
	// 未确认消息总量 = Prefetch × ConsumerWorkers，调大前者时要同步考虑内存与重复投递面。
	ConsumerWorkers int `yaml:"consumer_workers"`
}

type ClusterConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Shards     int    `yaml:"shards"`
	BusChannel string `yaml:"bus_channel"`
}

// RateLimitConfig 限流配置。
//
// 两类档位：
//   - 读接口只有进程内令牌桶，零网络开销；
//   - 登录与写接口两道闸串联 —— 本地令牌桶管「此刻能打多快」，
//     Redis 滑动窗口管「这段时间总共能用多少」，后者跨实例共享，
//     集群扩容不会顺带放宽配额。Redis 故障时自动退化为只有令牌桶。
type RateLimitConfig struct {
	Enabled     bool `yaml:"enabled"`
	Distributed bool `yaml:"distributed"` // true 时关键接口叠加 Redis 滑动窗口

	HTTPQPS   float64 `yaml:"http_qps"` // 读接口：单用户每秒
	HTTPBurst int     `yaml:"http_burst"`

	WriteQPS    float64  `yaml:"write_qps"` // 写接口瞬时速率：单用户每秒
	WriteBurst  int      `yaml:"write_burst"`
	WriteLimit  int64    `yaml:"write_limit"`  // 写接口总量：单用户每窗口上限，跨实例共享
	WriteWindow Duration `yaml:"write_window"` // 写接口滑动窗口长度

	AuthQPS    float64  `yaml:"auth_qps"` // 登录/注册瞬时速率：单 IP 每秒
	AuthBurst  int      `yaml:"auth_burst"`
	AuthLimit  int64    `yaml:"auth_limit"`  // 登录总量：单 IP 每窗口尝试次数，跨实例共享
	AuthWindow Duration `yaml:"auth_window"` // 登录滑动窗口长度

	UploadQPS   float64 `yaml:"upload_qps"` // 上传：单用户每秒
	UploadBurst int     `yaml:"upload_burst"`

	WSHandshakeQPS   float64 `yaml:"ws_handshake_qps"` // WS 握手：单 IP 每秒
	WSHandshakeBurst int     `yaml:"ws_handshake_burst"`
	WSMaxConns       int     `yaml:"ws_max_conns"` // 单用户最大在线设备数

	WSMsgQPS   float64 `yaml:"ws_msg_qps"` // WS 上行：单连接每秒
	WSMsgBurst int     `yaml:"ws_msg_burst"`
}

type CacheConfig struct {
	UserTTL        Duration `yaml:"user_ttl"`
	FriendTTL      Duration `yaml:"friend_ttl"`
	GroupMemberTTL Duration `yaml:"group_member_ttl"`
	TTLJitter      Duration `yaml:"ttl_jitter"`
}

type JWTConfig struct {
	Secret string   `yaml:"secret"`
	Expire Duration `yaml:"expire"`
	Issuer string   `yaml:"issuer"`
}

// UploadConfig 是图片存储配置。
//
// 只有一种介质：阿里云 OSS。图片由后端直传到桶里，对外地址再经 CDN 回源，
// 后端不再保留本地图片目录，也不再挂载静态资源路由。
type UploadConfig struct {
	MaxSizeMB int `yaml:"max_size_mb"`
	// Timeout 是单次上传的请求预算，与普通接口的 15s 分开。
	//
	// 两者约束的是完全不同的东西：普通接口的 15s 是「慢查询不能拖垮连接池」，
	// 而上传要传的是真实的 10MB 字节。按 15s 算，客户端上行带宽得稳定在
	// 5.3Mbps 以上，移动网络上传一张原图就直接超时——前端看到的是「网络错误」，
	// 而后端日志里什么都看不到（连接在读完 body 前就断了）。
	Timeout Duration `yaml:"timeout"`
	// PublicBase 是图片对外的访问前缀：接了 CDN 就填 CDN 域名，
	// 没接时留空，按 https://{bucket}.{endpoint} 推导。
	PublicBase string    `yaml:"public_base"`
	OSS        OSSConfig `yaml:"oss"`
}

// OSSConfig 是阿里云对象存储参数。
//
// 密钥不放 yaml：这份文件本身在仓库里，凭据一律由
// IMCHAT_OSS_ACCESS_KEY_ID / IMCHAT_OSS_ACCESS_KEY_SECRET 注入。
type OSSConfig struct {
	// Endpoint 是 bucket 所在区域的访问域名，不带 bucket 前缀，
	// 形如 oss-cn-beijing.aliyuncs.com（协议头可省略，缺省按 https）。
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
	// ACL 是写入对象的访问控制，取值 private / public-read /
	// public-read-write / default，留空表示不显式设置，
	// 让对象跟随桶的默认策略。
	//
	// 桶是私有的、图片却要按 URL 直读时，这里必须填 public-read，
	// 否则上传能成功、拿到的地址却是 403。
	ACL string `yaml:"acl"`
}

// MessageConfig 消息相关业务参数。
type MessageConfig struct {
	// RecallWindow 撤回时间窗，只约束「撤回自己发的消息」；
	// 群管理员撤回他人消息不受它限制，否则违规内容过了窗口就再也处理不掉。
	RecallWindow Duration `yaml:"recall_window"`
}

// Config 是全局配置根对象。
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	MySQL     MySQLConfig     `yaml:"mysql"`
	Redis     RedisConfig     `yaml:"redis"`
	RabbitMQ  RabbitMQConfig  `yaml:"rabbitmq"`
	Cluster   ClusterConfig   `yaml:"cluster"`
	RateLimit RateLimitConfig `yaml:"ratelimit"`
	Cache     CacheConfig     `yaml:"cache"`
	JWT       JWTConfig       `yaml:"jwt"`
	Upload    UploadConfig    `yaml:"upload"`
	Message   MessageConfig   `yaml:"message"`
}

// Load 读取配置文件，随后用 IMCHAT_* 环境变量覆盖关键项，方便容器部署。
func Load(path string) (*Config, error) {
	if err := loadDotEnv(dotEnvFile); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.applyDefaults()
	cfg.applyEnv()
	// 派生值必须在 applyEnv 之后算：bucket / endpoint 常由环境变量注入，
	// 放在 applyDefaults 里会拿到空串，拼出一个只有协议头的前缀。
	cfg.derive()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.Mode == "" {
		c.Server.Mode = "debug"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(15 * time.Second)
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = Duration(15 * time.Second)
	}
	if c.Server.WSPingInterval == 0 {
		c.Server.WSPingInterval = Duration(30 * time.Second)
	}
	if c.Server.WSPongWait == 0 {
		c.Server.WSPongWait = Duration(75 * time.Second)
	}
	if c.Server.WSWriteWait == 0 {
		c.Server.WSWriteWait = Duration(10 * time.Second)
	}
	if c.Server.WSSendBuffer == 0 {
		c.Server.WSSendBuffer = 512
	}
	if c.Server.GracefulTimeout == 0 {
		c.Server.GracefulTimeout = Duration(15 * time.Second)
	}

	if c.MySQL.MaxIdleConns == 0 {
		c.MySQL.MaxIdleConns = 20
	}
	if c.MySQL.MaxOpenConns == 0 {
		c.MySQL.MaxOpenConns = 200
	}
	if c.MySQL.ConnMaxLifetime == 0 {
		c.MySQL.ConnMaxLifetime = Duration(30 * time.Minute)
	}
	if c.MySQL.ConnMaxIdleTime == 0 {
		c.MySQL.ConnMaxIdleTime = Duration(5 * time.Minute)
	}

	if c.Redis.PoolSize == 0 {
		c.Redis.PoolSize = 200
	}
	if c.Redis.MinIdleConns == 0 {
		c.Redis.MinIdleConns = 20
	}
	if c.Redis.DialTimeout == 0 {
		c.Redis.DialTimeout = Duration(2 * time.Second)
	}
	if c.Redis.ReadTimeout == 0 {
		c.Redis.ReadTimeout = Duration(time.Second)
	}
	if c.Redis.WriteTimeout == 0 {
		c.Redis.WriteTimeout = Duration(time.Second)
	}

	if c.RabbitMQ.Prefetch == 0 {
		c.RabbitMQ.Prefetch = 100
	}
	if c.RabbitMQ.ChannelPool == 0 {
		c.RabbitMQ.ChannelPool = 8
	}
	if c.RabbitMQ.ConsumerWorkers == 0 {
		c.RabbitMQ.ConsumerWorkers = 3
	}

	if c.Cluster.Shards == 0 {
		c.Cluster.Shards = 64
	}
	if c.Cluster.BusChannel == "" {
		c.Cluster.BusChannel = "im:bus"
	}

	if c.RateLimit.HTTPQPS == 0 {
		c.RateLimit.HTTPQPS = 50
	}
	if c.RateLimit.HTTPBurst == 0 {
		c.RateLimit.HTTPBurst = 100
	}
	if c.RateLimit.WriteQPS == 0 {
		c.RateLimit.WriteQPS = 10
	}
	if c.RateLimit.WriteBurst == 0 {
		c.RateLimit.WriteBurst = 20
	}
	if c.RateLimit.WriteLimit == 0 {
		c.RateLimit.WriteLimit = 120
	}
	if c.RateLimit.WriteWindow == 0 {
		c.RateLimit.WriteWindow = Duration(time.Minute)
	}
	if c.RateLimit.AuthQPS == 0 {
		c.RateLimit.AuthQPS = 5
	}
	if c.RateLimit.AuthBurst == 0 {
		c.RateLimit.AuthBurst = 10
	}
	if c.RateLimit.AuthLimit == 0 {
		c.RateLimit.AuthLimit = 20
	}
	if c.RateLimit.AuthWindow == 0 {
		c.RateLimit.AuthWindow = Duration(time.Minute)
	}
	if c.RateLimit.UploadQPS == 0 {
		c.RateLimit.UploadQPS = 2
	}
	if c.RateLimit.UploadBurst == 0 {
		c.RateLimit.UploadBurst = 5
	}
	if c.RateLimit.WSHandshakeQPS == 0 {
		c.RateLimit.WSHandshakeQPS = 2
	}
	if c.RateLimit.WSHandshakeBurst == 0 {
		c.RateLimit.WSHandshakeBurst = 4
	}
	if c.RateLimit.WSMaxConns == 0 {
		c.RateLimit.WSMaxConns = 5
	}
	if c.RateLimit.WSMsgQPS == 0 {
		c.RateLimit.WSMsgQPS = 20
	}
	if c.RateLimit.WSMsgBurst == 0 {
		c.RateLimit.WSMsgBurst = 40
	}

	if c.Cache.UserTTL == 0 {
		c.Cache.UserTTL = Duration(10 * time.Minute)
	}
	if c.Cache.FriendTTL == 0 {
		c.Cache.FriendTTL = Duration(5 * time.Minute)
	}
	if c.Cache.GroupMemberTTL == 0 {
		c.Cache.GroupMemberTTL = Duration(5 * time.Minute)
	}
	if c.Cache.TTLJitter == 0 {
		c.Cache.TTLJitter = Duration(time.Minute)
	}

	if c.JWT.Expire == 0 {
		c.JWT.Expire = Duration(168 * time.Hour)
	}
	if c.JWT.Issuer == "" {
		c.JWT.Issuer = "imchat"
	}

	// upload.public_base 不在这里给默认值：它依赖 bucket 与 endpoint，
	// 而那两个字段常常由环境变量注入，留给 derive 在 applyEnv 之后推导。
	if c.Upload.MaxSizeMB == 0 {
		c.Upload.MaxSizeMB = 10
	}
	// 10MB 在 1.4Mbps 上要 60s，给到 120s 才能在弱网下把「文件太大」与
	// 「网速太慢」区分开；再长就是在给一个已经死掉的连接占着资源。
	if c.Upload.Timeout == 0 {
		c.Upload.Timeout = Duration(120 * time.Second)
	}

	if c.Message.RecallWindow == 0 {
		c.Message.RecallWindow = Duration(2 * time.Minute)
	}
}

// applyEnv 只覆盖最常变更的几项，避免引入配置中心依赖。
func (c *Config) applyEnv() {
	setStr(&c.Server.Addr, "IMCHAT_SERVER_ADDR")
	setStr(&c.MySQL.DSN, "IMCHAT_MYSQL_DSN")
	setStr(&c.Redis.Addr, "IMCHAT_REDIS_ADDR")
	setStr(&c.Redis.Password, "IMCHAT_REDIS_PASSWORD")
	setStr(&c.RabbitMQ.URL, "IMCHAT_RABBITMQ_URL")
	setStr(&c.JWT.Secret, "IMCHAT_JWT_SECRET")
	setStr(&c.Upload.PublicBase, "IMCHAT_UPLOAD_PUBLIC_BASE")
	// OSS 的接入点与凭据全部走环境变量：写进 yaml 等于跟着仓库一起泄露，
	// 而且换桶、换密钥时不该产生一次配置文件改动。
	setStr(&c.Upload.OSS.Endpoint, "IMCHAT_OSS_ENDPOINT")
	setStr(&c.Upload.OSS.Bucket, "IMCHAT_OSS_BUCKET")
	setStr(&c.Upload.OSS.AccessKeyID, "IMCHAT_OSS_ACCESS_KEY_ID")
	setStr(&c.Upload.OSS.AccessKeySecret, "IMCHAT_OSS_ACCESS_KEY_SECRET")
	setStr(&c.Upload.OSS.ACL, "IMCHAT_OSS_ACL")
}

// derive 计算依赖其它字段的派生值。
//
// 只能在 applyEnv 之后跑：endpoint / bucket 常由环境变量注入，
// 在默认值阶段推导会拿到空串，拼出一个只有协议头的前缀。
func (c *Config) derive() {
	// 图片访问前缀：没配 CDN 时按 OSS 的默认 bucket 域名规则拼
	// https://{bucket}.{endpoint}。仍然留空说明 endpoint/bucket 也没配，
	// 交给 validate 报「必填项为空」，不在这里编一个假地址。
	if c.Upload.PublicBase != "" || c.Upload.OSS.Bucket == "" || c.Upload.OSS.Endpoint == "" {
		return
	}
	c.Upload.PublicBase = "https://" + c.Upload.OSS.Bucket + "." + trimScheme(c.Upload.OSS.Endpoint)
}

func trimScheme(endpoint string) string {
	for _, s := range []string{"https://", "http://"} {
		if strings.HasPrefix(endpoint, s) {
			return strings.TrimPrefix(endpoint, s)
		}
	}
	return endpoint
}

func setStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func (c *Config) validate() error {
	if c.MySQL.DSN == "" {
		return fmt.Errorf("mysql.dsn 不能为空")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr 不能为空")
	}
	if len(c.JWT.Secret) < 16 {
		return fmt.Errorf("jwt.secret 长度至少 16 位")
	}
	// 监听地址为空时 http.Server 会退化成默认的 ":http"（80 端口）而不是报错，
	// 等到端口被占用或权限不足才发现配错。applyDefaults 已兜过空值，
	// 这里挡的是绕过默认值手工构造的配置。
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	// 读截止时间靠 Pong 续期，而 Pong 是对 Ping 的应答。pong_wait 不大于 ping 间隔时，
	// 第一条 Ping 都还没发出去，读截止时间就先到了，所有连接建好即被判死
	// （现象是客户端连上就断、服务端只有一条读超时，看不出与配置有关）。
	if c.Server.WSPongWait.Get() <= c.Server.WSPingInterval.Get() {
		return fmt.Errorf("server.ws_pong_wait(%s) 必须大于 ws_ping_interval(%s)",
			c.Server.WSPongWait.Get(), c.Server.WSPingInterval.Get())
	}
	// 写截止时间是 now + write_wait：为 0 或负数时每一帧（含心跳）都立刻超时，
	// 表现为连接刚建就被关掉。
	if c.Server.WSWriteWait.Get() <= 0 {
		return fmt.Errorf("server.ws_write_wait 必须为正数，当前 %s", c.Server.WSWriteWait.Get())
	}
	if c.Cluster.Shards <= 0 || c.Cluster.Shards&(c.Cluster.Shards-1) != 0 {
		return fmt.Errorf("cluster.shards 必须是 2 的幂，当前 %d", c.Cluster.Shards)
	}
	// 每个 worker 独占一个 channel，配成几百个会直接打爆 broker 的连接与内存。
	// 0 在这里表示「没填」，由 applyDefaults 归一成默认值，报错文案按这个真实范围写。
	if c.RabbitMQ.ConsumerWorkers < 0 || c.RabbitMQ.ConsumerWorkers > maxConsumerWorkers {
		return fmt.Errorf("rabbitmq.consumer_workers 取值 0~%d（0 表示未填，默认 3），当前 %d",
			maxConsumerWorkers, c.RabbitMQ.ConsumerWorkers)
	}
	// 滑动窗口少了窗口长度就等于没有配额，配置漏填要在启动期就报出来，
	// 不能等线上跑起来才发现某个接口全程 429。
	if c.RateLimit.Enabled && c.RateLimit.Distributed {
		for _, r := range []struct {
			name   string
			limit  int64
			window time.Duration
		}{
			{"write", c.RateLimit.WriteLimit, c.RateLimit.WriteWindow.Get()},
			{"auth", c.RateLimit.AuthLimit, c.RateLimit.AuthWindow.Get()},
		} {
			if r.limit <= 0 || r.window <= 0 {
				return fmt.Errorf("ratelimit 启用 distributed 时 %s 的 limit/window 必须为正数", r.name)
			}
		}
	}
	// 上传上限会直接变成 http.MaxBytesReader 的 n：负数被标准库当成 0，
	// 于是每一次上传都失败，而报错文案还是「不能超过 -1MB」；上百 MB 则
	// 单请求就要把整个文件过一遍内存与带宽，等于把资源交给一个配置笔误。
	if c.Upload.MaxSizeMB < 1 || c.Upload.MaxSizeMB > maxUploadSizeMB {
		return fmt.Errorf("upload.max_size_mb 取值 1~%d，当前 %d", maxUploadSizeMB, c.Upload.MaxSizeMB)
	}
	// OSS 三件套缺任何一项，服务都照样能启动，直到第一个用户上传才失败，
	// 而那时前端只看到「文件保存失败」。这类配置错误必须在启动期暴露。
	if c.Upload.OSS.Endpoint == "" || c.Upload.OSS.Bucket == "" {
		return fmt.Errorf("upload.oss.endpoint 与 upload.oss.bucket 不能为空")
	}
	if c.Upload.OSS.AccessKeyID == "" || c.Upload.OSS.AccessKeySecret == "" {
		return fmt.Errorf("upload.oss 凭据不能为空，请注入 IMCHAT_OSS_ACCESS_KEY_ID / IMCHAT_OSS_ACCESS_KEY_SECRET")
	}
	// ACL 写错不会报错，只会让 SDK 把这个字符串原样发给 OSS，
	// 而 OSS 对非法值回的错误文案是「InvalidArgument」+ 内部字段名，
	// 排障时很难联想到是配置笔误。
	switch c.Upload.OSS.ACL {
	case "", "default", "private", "public-read", "public-read-write":
	default:
		return fmt.Errorf("upload.oss.acl 只能是 private / public-read / public-read-write / default，当前 %q", c.Upload.OSS.ACL)
	}
	if c.Upload.Timeout <= 0 {
		return fmt.Errorf("upload.timeout 必须为正数，当前 %s", c.Upload.Timeout.Get())
	}
	// 对外前缀必须是绝对地址：写成 "cdn.example.com" 这种裸域名会被前端当成
	// 当前站点下的相对路径，图片静默全部 404，而接口返回的字段看起来完全正常。
	if !strings.HasPrefix(c.Upload.PublicBase, "https://") && !strings.HasPrefix(c.Upload.PublicBase, "http://") {
		return fmt.Errorf("upload.public_base 必须是 http(s):// 开头的绝对地址，当前 %q", c.Upload.PublicBase)
	}
	// 撤回窗口写成负数等于「永远不许撤回」，多半是配置笔误，启动期就拦下来
	if c.Message.RecallWindow < 0 {
		return fmt.Errorf("message.recall_window 不能为负数，当前 %s", c.Message.RecallWindow.Get())
	}
	return nil
}
