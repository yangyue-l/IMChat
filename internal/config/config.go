// Package config 负责加载 YAML 配置并支持环境变量覆盖。
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 yaml 支持 "30s" / "1h" 这类时长写法。
type Duration time.Duration

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

type UploadConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	PublicBase string `yaml:"public_base"`
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
}

// Load 读取配置文件，随后用 IMCHAT_* 环境变量覆盖关键项，方便容器部署。
func Load(path string) (*Config, error) {
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

	if c.Upload.Dir == "" {
		c.Upload.Dir = "./uploads"
	}
	if c.Upload.MaxSizeMB == 0 {
		c.Upload.MaxSizeMB = 10
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
	if c.Cluster.Shards <= 0 || c.Cluster.Shards&(c.Cluster.Shards-1) != 0 {
		return fmt.Errorf("cluster.shards 必须是 2 的幂，当前 %d", c.Cluster.Shards)
	}
	// 每个 worker 独占一个 channel，配成几百个会直接打爆 broker 的连接与内存
	if c.RabbitMQ.ConsumerWorkers < 0 || c.RabbitMQ.ConsumerWorkers > maxConsumerWorkers {
		return fmt.Errorf("rabbitmq.consumer_workers 取值 1~%d，当前 %d",
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
	return nil
}
