package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// envKeys 是 applyEnv 认的全部环境变量。
//
// 每个走 Load 的用例都先清空它们：本机 shell 里若残留 IMCHAT_JWT_SECRET 之类的变量，
// 「缺省值」用例会随环境随机失败，而失败信息完全指不到原因。
var envKeys = []string{
	"IMCHAT_SERVER_ADDR",
	"IMCHAT_MYSQL_DSN",
	"IMCHAT_REDIS_ADDR",
	"IMCHAT_REDIS_PASSWORD",
	"IMCHAT_RABBITMQ_URL",
	"IMCHAT_JWT_SECRET",
	"IMCHAT_UPLOAD_PUBLIC_BASE",
	"IMCHAT_OSS_ENDPOINT",
	"IMCHAT_OSS_BUCKET",
	"IMCHAT_OSS_ACCESS_KEY_ID",
	"IMCHAT_OSS_ACCESS_KEY_SECRET",
}

// clearEnv 把 applyEnv 认的变量全部置空（setStr 忽略空串，等价于没设）。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
}

// writeConfig 把 yaml 正文写进临时目录并返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

// minimalYAML 是能通过 validate 的最小配置：必填项写全，其余全交给默认值。
//
// OSS 段必须出现：图片只有对象存储一种介质，缺 endpoint/bucket/凭据
// 在启动期就会被 validate 拦下。这里的 AK/SK 是假值，真实凭据走环境变量。
const minimalYAML = `
mysql:
  dsn: "root:pwd@tcp(127.0.0.1:3306)/imchat?charset=utf8mb4&parseTime=True"
redis:
  addr: "127.0.0.1:6379"
jwt:
  secret: "0123456789abcdef"
upload:
  oss:
    endpoint: "oss-cn-hangzhou.aliyuncs.com"
    bucket: "imchat-test"
    access_key_id: "test-ak"
    access_key_secret: "test-sk"
`

// baseConfig 返回一份能通过 validate 的内存配置。
// validate 用例直接改字段，不经过 Load —— 否则 applyDefaults 会先把 0 值填成默认值，
// 「shards 为 0」「window 为 0」这类分支就永远测不到。
func baseConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:           ":8080",
			WSPingInterval: Duration(30 * time.Second),
			WSPongWait:     Duration(75 * time.Second),
			WSWriteWait:    Duration(10 * time.Second),
		},
		MySQL:    MySQLConfig{DSN: "root:pwd@tcp(127.0.0.1:3306)/imchat"},
		Redis:    RedisConfig{Addr: "127.0.0.1:6379"},
		JWT:      JWTConfig{Secret: "0123456789abcdef"},
		Cluster:  ClusterConfig{Shards: 64},
		RabbitMQ: RabbitMQConfig{ConsumerWorkers: 3},
		RateLimit: RateLimitConfig{
			Enabled:     true,
			Distributed: true,
			WriteLimit:  120,
			WriteWindow: Duration(time.Minute),
			AuthLimit:   20,
			AuthWindow:  Duration(time.Minute),
		},
		Upload: UploadConfig{
			MaxSizeMB:  10,
			Timeout:    Duration(120 * time.Second),
			PublicBase: "https://cdn.example.com",
			OSS: OSSConfig{
				Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
				Bucket:          "imchat-test",
				AccessKeyID:     "ak",
				AccessKeySecret: "sk",
			},
		},
		Message: MessageConfig{RecallWindow: Duration(2 * time.Minute)},
	}
}

// TestDurationUnmarshal 覆盖 Duration 的合法与非法写法。
func TestDurationUnmarshal(t *testing.T) {
	type holder struct {
		D Duration `yaml:"d"`
	}
	cases := []struct {
		name    string
		body    string
		want    time.Duration
		wantErr string
	}{
		{"秒", "d: 30s", 30 * time.Second, ""},
		{"复合写法", "d: 1h30m", 90 * time.Minute, ""},
		{"空串当未填", `d: ""`, 0, ""},
		{"null 当未填", "d:", 0, ""},
		{"字段缺失", "x: 1", 0, ""},
		// 少了单位会按 ParseDuration 拒绝。这一条容易踩：yaml 里的裸数字能被
		// Decode 成字符串 "30"，所以错误来自 ParseDuration 而不是 yaml 解码，
		// 报错信息里必须带上原始值，否则用户不知道该改哪一行。
		{"缺单位", "d: 30", 0, `"30"`},
		{"非法单位", "d: 30x", 0, `"30x"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h holder
			err := yaml.Unmarshal([]byte(c.body), &h)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错，实际解析成 %v", time.Duration(h.D))
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("报错信息应包含 %q，实际 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got := h.D.Get(); got != c.want {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
		})
	}
}

// TestLoadAppliesDefaults 确认「只填必填项」时所有缺省值都落位。
//
// 该表是 applyDefaults 的镜像：新增带默认值的字段时要同步往这里加一行，
// 否则「加了字段忘了默认值」在启动时只会表现为某处行为异常，很难定位。
func TestLoadAppliesDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"server.addr", cfg.Server.Addr, ":8080"},
		{"server.mode", cfg.Server.Mode, "debug"},
		{"server.read_timeout", cfg.Server.ReadTimeout.Get(), 15 * time.Second},
		{"server.write_timeout", cfg.Server.WriteTimeout.Get(), 15 * time.Second},
		{"server.ws_ping_interval", cfg.Server.WSPingInterval.Get(), 30 * time.Second},
		{"server.ws_pong_wait", cfg.Server.WSPongWait.Get(), 75 * time.Second},
		{"server.ws_write_wait", cfg.Server.WSWriteWait.Get(), 10 * time.Second},
		{"server.ws_send_buffer", cfg.Server.WSSendBuffer, 512},
		{"server.graceful_timeout", cfg.Server.GracefulTimeout.Get(), 15 * time.Second},

		{"mysql.max_idle_conns", cfg.MySQL.MaxIdleConns, 20},
		{"mysql.max_open_conns", cfg.MySQL.MaxOpenConns, 200},
		{"mysql.conn_max_lifetime", cfg.MySQL.ConnMaxLifetime.Get(), 30 * time.Minute},
		{"mysql.conn_max_idle_time", cfg.MySQL.ConnMaxIdleTime.Get(), 5 * time.Minute},

		{"redis.pool_size", cfg.Redis.PoolSize, 200},
		{"redis.min_idle_conns", cfg.Redis.MinIdleConns, 20},
		{"redis.dial_timeout", cfg.Redis.DialTimeout.Get(), 2 * time.Second},
		{"redis.read_timeout", cfg.Redis.ReadTimeout.Get(), time.Second},
		{"redis.write_timeout", cfg.Redis.WriteTimeout.Get(), time.Second},

		{"rabbitmq.prefetch", cfg.RabbitMQ.Prefetch, 100},
		{"rabbitmq.channel_pool", cfg.RabbitMQ.ChannelPool, 8},
		{"rabbitmq.consumer_workers", cfg.RabbitMQ.ConsumerWorkers, 3},

		{"cluster.shards", cfg.Cluster.Shards, 64},
		{"cluster.bus_channel", cfg.Cluster.BusChannel, "im:bus"},

		{"ratelimit.http_qps", cfg.RateLimit.HTTPQPS, 50.0},
		{"ratelimit.http_burst", cfg.RateLimit.HTTPBurst, 100},
		{"ratelimit.write_qps", cfg.RateLimit.WriteQPS, 10.0},
		{"ratelimit.write_burst", cfg.RateLimit.WriteBurst, 20},
		{"ratelimit.write_limit", cfg.RateLimit.WriteLimit, int64(120)},
		{"ratelimit.write_window", cfg.RateLimit.WriteWindow.Get(), time.Minute},
		{"ratelimit.auth_qps", cfg.RateLimit.AuthQPS, 5.0},
		{"ratelimit.auth_burst", cfg.RateLimit.AuthBurst, 10},
		{"ratelimit.auth_limit", cfg.RateLimit.AuthLimit, int64(20)},
		{"ratelimit.auth_window", cfg.RateLimit.AuthWindow.Get(), time.Minute},
		{"ratelimit.upload_qps", cfg.RateLimit.UploadQPS, 2.0},
		{"ratelimit.upload_burst", cfg.RateLimit.UploadBurst, 5},
		{"ratelimit.ws_handshake_qps", cfg.RateLimit.WSHandshakeQPS, 2.0},
		{"ratelimit.ws_handshake_burst", cfg.RateLimit.WSHandshakeBurst, 4},
		{"ratelimit.ws_max_conns", cfg.RateLimit.WSMaxConns, 5},
		{"ratelimit.ws_msg_qps", cfg.RateLimit.WSMsgQPS, 20.0},
		{"ratelimit.ws_msg_burst", cfg.RateLimit.WSMsgBurst, 40},

		{"cache.user_ttl", cfg.Cache.UserTTL.Get(), 10 * time.Minute},
		{"cache.friend_ttl", cfg.Cache.FriendTTL.Get(), 5 * time.Minute},
		{"cache.group_member_ttl", cfg.Cache.GroupMemberTTL.Get(), 5 * time.Minute},
		{"cache.ttl_jitter", cfg.Cache.TTLJitter.Get(), time.Minute},

		{"jwt.expire", cfg.JWT.Expire.Get(), 168 * time.Hour},
		{"jwt.issuer", cfg.JWT.Issuer, "imchat"},

		{"upload.max_size_mb", cfg.Upload.MaxSizeMB, 10},
		// 上传预算必须与 server 的 15s 分开：10MB 在弱网上 15s 传不完。
		{"upload.timeout", cfg.Upload.Timeout.Get(), 120 * time.Second},
		// public_base 没配时要按 {bucket}.{endpoint} 推出来，
		// 否则 storage.New 会拿到空前缀，返回的图片地址全是相对路径。
		{"upload.public_base 推导", cfg.Upload.PublicBase, "https://imchat-test.oss-cn-hangzhou.aliyuncs.com"},

		{"message.recall_window", cfg.Message.RecallWindow.Get(), 2 * time.Minute},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s 期望 %v，实际 %v", c.name, c.want, c.got)
		}
	}
}

// TestLoadKeepsExplicitValues 确认默认值不会盖掉配置文件里写明的值。
func TestLoadKeepsExplicitValues(t *testing.T) {
	clearEnv(t)
	// 单独拼一份而不是在 minimalYAML 后面追加：yaml 里重复出现的同名段是整段覆盖，
	// 追加写法会让人以为两边的字段会合并。
	body := `
server:
  addr: ":9999"
  ws_send_buffer: 64
  pprof: true
mysql:
  dsn: "root:pwd@tcp(127.0.0.1:3306)/imchat"
redis:
  addr: "127.0.0.1:6379"
jwt:
  secret: "0123456789abcdef"
upload:
  oss:
    endpoint: "oss-cn-hangzhou.aliyuncs.com"
    bucket: "imchat-test"
    access_key_id: "test-ak"
    access_key_secret: "test-sk"
cluster:
  shards: 4
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Fatalf("server.addr 被默认值覆盖: %q", cfg.Server.Addr)
	}
	if cfg.Server.WSSendBuffer != 64 {
		t.Fatalf("server.ws_send_buffer 期望 64，实际 %d", cfg.Server.WSSendBuffer)
	}
	if !cfg.Server.Pprof {
		t.Fatal("server.pprof 显式写 true 却仍是 false")
	}
	if cfg.Cluster.Shards != 4 {
		t.Fatalf("cluster.shards 期望 4，实际 %d", cfg.Cluster.Shards)
	}
	// 未写的同级字段仍拿默认值
	if cfg.Server.WSPongWait.Get() != 75*time.Second {
		t.Fatalf("未写的 ws_pong_wait 应取默认值，实际 %v", cfg.Server.WSPongWait.Get())
	}
}

// TestLoadEnvOverrides 覆盖 applyEnv 的全部变量，并确认空值不生效。
//
// 这条边界很实际：容器编排常用「变量声明了但值留空」表示不覆盖，
// 若 setStr 把空串也写进去，默认值会被抹成空。
func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	want := map[string]string{
		"IMCHAT_SERVER_ADDR":           ":7070",
		"IMCHAT_MYSQL_DSN":             "env-dsn",
		"IMCHAT_REDIS_ADDR":            "10.0.0.1:6379",
		"IMCHAT_REDIS_PASSWORD":        "s3cret",
		"IMCHAT_RABBITMQ_URL":          "amqp://guest:guest@10.0.0.2:5672/",
		"IMCHAT_JWT_SECRET":            "0123456789abcdef-env",
		"IMCHAT_UPLOAD_PUBLIC_BASE":    "https://cdn.example.com",
		"IMCHAT_OSS_ENDPOINT":          "oss-cn-beijing.aliyuncs.com",
		"IMCHAT_OSS_BUCKET":            "env-bucket",
		"IMCHAT_OSS_ACCESS_KEY_ID":     "env-ak",
		"IMCHAT_OSS_ACCESS_KEY_SECRET": "env-sk",
	}
	for k, v := range want {
		t.Setenv(k, v)
	}
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	got := map[string]string{
		"IMCHAT_SERVER_ADDR":           cfg.Server.Addr,
		"IMCHAT_MYSQL_DSN":             cfg.MySQL.DSN,
		"IMCHAT_REDIS_ADDR":            cfg.Redis.Addr,
		"IMCHAT_REDIS_PASSWORD":        cfg.Redis.Password,
		"IMCHAT_RABBITMQ_URL":          cfg.RabbitMQ.URL,
		"IMCHAT_JWT_SECRET":            cfg.JWT.Secret,
		"IMCHAT_UPLOAD_PUBLIC_BASE":    cfg.Upload.PublicBase,
		"IMCHAT_OSS_ENDPOINT":          cfg.Upload.OSS.Endpoint,
		"IMCHAT_OSS_BUCKET":            cfg.Upload.OSS.Bucket,
		"IMCHAT_OSS_ACCESS_KEY_ID":     cfg.Upload.OSS.AccessKeyID,
		"IMCHAT_OSS_ACCESS_KEY_SECRET": cfg.Upload.OSS.AccessKeySecret,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 未生效：期望 %q，实际 %q", k, v, got[k])
		}
	}
}

// TestLoadEnvEmptyKeepsDefault 确认「变量存在但为空」不会把默认值抹掉。
func TestLoadEnvEmptyKeepsDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("IMCHAT_SERVER_ADDR", "")
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("空环境变量应保留默认值，实际 %q", cfg.Server.Addr)
	}
}

// TestLoadEnvFillsRequiredField 锁住 Load 的执行顺序：默认值 → 环境变量 → 校验。
//
// 顺序若反过来（先校验再覆盖环境），「配置文件不带 DSN、靠容器注入」的标准部署方式
// 会在启动时直接报「mysql.dsn 不能为空」。
func TestLoadEnvFillsRequiredField(t *testing.T) {
	clearEnv(t)
	t.Setenv("IMCHAT_MYSQL_DSN", "root:pwd@tcp(db:3306)/imchat")
	t.Setenv("IMCHAT_REDIS_ADDR", "redis:6379")
	t.Setenv("IMCHAT_JWT_SECRET", "0123456789abcdef")
	t.Setenv("IMCHAT_OSS_ENDPOINT", "oss-cn-hangzhou.aliyuncs.com")
	t.Setenv("IMCHAT_OSS_BUCKET", "imchat")
	t.Setenv("IMCHAT_OSS_ACCESS_KEY_ID", "ak")
	t.Setenv("IMCHAT_OSS_ACCESS_KEY_SECRET", "sk")

	// 故意只留空壳，必填项全靠环境变量
	cfg, err := Load(writeConfig(t, "server:\n  mode: release\n"))
	if err != nil {
		t.Fatalf("必填项应由环境变量补齐，实际报错: %v", err)
	}
	// 顺带锁住 derive 的执行时点：endpoint/bucket 来自环境变量，
	// 若在 applyEnv 之前推导 public_base，这里会拿到空串。
	if want := "https://imchat.oss-cn-hangzhou.aliyuncs.com"; cfg.Upload.PublicBase != want {
		t.Fatalf("public_base 应由环境变量注入的桶名推导，期望 %q，实际 %q", want, cfg.Upload.PublicBase)
	}
}

// TestLoadErrors 覆盖 Load 的四个失败出口。
func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string // 空表示用不存在的路径
		wantErr string
	}{
		{"文件不存在", "", "读取配置文件"},
		{"yaml 语法错", "server: [1,2", "解析配置文件失败"},
		{"时长非法", minimalYAML + "\nserver:\n  read_timeout: 15\n", "配置时长"},
		{"缺必填项", "jwt:\n  secret: \"0123456789abcdef\"\n", "mysql.dsn"},
		{"secret 太短", "mysql:\n  dsn: d\nredis:\n  addr: r\njwt:\n  secret: short\n", "jwt.secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			path := filepath.Join(t.TempDir(), "absent.yaml")
			if c.body != "" {
				path = writeConfig(t, c.body)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("期望报错，实际成功")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("报错信息应包含 %q，实际 %v", c.wantErr, err)
			}
		})
	}
}

// TestValidateErrorCarriesCurrentValue 锁住「每条报错都要带出当前值」。
//
// 只写「pong_wait 必须大于 ping 间隔」时，运维还得回头翻配置才知道是改前者还是后者；
// 带出两边值的报错才能直接定位到该改哪一行。
func TestValidateErrorCarriesCurrentValue(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   []string // 报错里必须同时出现的片段
	}{
		{"pong_wait 带出两边值", func(c *Config) {
			c.Server.WSPingInterval = Duration(30 * time.Second)
			c.Server.WSPongWait = Duration(10 * time.Second)
		}, []string{"10s", "30s"}},
		{"write_wait 带出当前值", func(c *Config) {
			c.Server.WSWriteWait = 0
		}, []string{"0s"}},
		{"max_size_mb 带出范围与当前值", func(c *Config) {
			c.Upload.MaxSizeMB = 200
		}, []string{"1~100", "200"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			c.mutate(cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("报错信息应包含 %q，实际 %v", w, err)
				}
			}
		})
	}
}

// TestValidate 逐条覆盖 validate 的拦截分支。
//
// 直接调用 validate 而不是 Load：applyDefaults 会把 0 值填成默认值，
// 「shards 为 0」「window 为 0」这类「配置漏填」的场景只有绕过默认值才构造得出来。
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // 空表示应当通过
	}{
		{"基线通过", nil, ""},

		{"dsn 为空", func(c *Config) { c.MySQL.DSN = "" }, "mysql.dsn"},
		{"redis addr 为空", func(c *Config) { c.Redis.Addr = "" }, "redis.addr"},
		{"secret 15 位", func(c *Config) { c.JWT.Secret = "0123456789abcde" }, "jwt.secret"},
		{"secret 恰好 16 位通过", func(c *Config) { c.JWT.Secret = "0123456789abcdef" }, ""},

		{"server.addr 为空", func(c *Config) { c.Server.Addr = "" }, "server.addr"},
		// 读截止时间等于 ping 间隔时，第一条 Ping 与读超时同时到，连接几乎必然被判死
		{"ws_pong_wait 等于 ping 间隔", func(c *Config) {
			c.Server.WSPongWait = c.Server.WSPingInterval
		}, "ws_pong_wait"},
		{"ws_pong_wait 小于 ping 间隔", func(c *Config) {
			c.Server.WSPongWait = Duration(5 * time.Second)
		}, "ws_pong_wait"},
		{"ws_pong_wait 略大于 ping 间隔通过", func(c *Config) {
			c.Server.WSPongWait = Duration(31 * time.Second)
		}, ""},
		{"ws_write_wait 为 0", func(c *Config) { c.Server.WSWriteWait = 0 }, "ws_write_wait"},
		{"ws_write_wait 负数", func(c *Config) {
			c.Server.WSWriteWait = Duration(-time.Second)
		}, "ws_write_wait"},

		{"shards 为 0", func(c *Config) { c.Cluster.Shards = 0 }, "cluster.shards"},
		{"shards 为 1 通过", func(c *Config) { c.Cluster.Shards = 1 }, ""},
		{"shards 非 2 的幂", func(c *Config) { c.Cluster.Shards = 48 }, "cluster.shards"},
		{"shards 负数", func(c *Config) { c.Cluster.Shards = -8 }, "cluster.shards"},

		{"consumer_workers 负数", func(c *Config) { c.RabbitMQ.ConsumerWorkers = -1 }, "consumer_workers"},
		{"consumer_workers 超上限", func(c *Config) { c.RabbitMQ.ConsumerWorkers = 33 }, "consumer_workers"},
		{"consumer_workers 上限 32 通过", func(c *Config) { c.RabbitMQ.ConsumerWorkers = 32 }, ""},
		// 0 被当作「没填」放过，交给 applyDefaults 归一成 3（报错文案已按 0~32 写）
		{"consumer_workers 为 0 视为未填", func(c *Config) { c.RabbitMQ.ConsumerWorkers = 0 }, ""},

		{"distributed 缺 write window", func(c *Config) { c.RateLimit.WriteWindow = 0 }, "write"},
		{"distributed 缺 write limit", func(c *Config) { c.RateLimit.WriteLimit = 0 }, "write"},
		{"distributed 缺 auth window", func(c *Config) { c.RateLimit.AuthWindow = 0 }, "auth"},
		{"distributed 缺 auth limit", func(c *Config) { c.RateLimit.AuthLimit = 0 }, "auth"},
		{"未启用 distributed 不校验窗口", func(c *Config) {
			c.RateLimit.Enabled = false
			c.RateLimit.WriteLimit = 0
			c.RateLimit.WriteWindow = 0
			c.RateLimit.AuthLimit = 0
			c.RateLimit.AuthWindow = 0
		}, ""},
		{"只启用 distributed 但整体关闭", func(c *Config) {
			c.RateLimit.Enabled = false
			c.RateLimit.WriteWindow = 0
		}, ""},

		{"upload.max_size_mb 为 0", func(c *Config) { c.Upload.MaxSizeMB = 0 }, "upload.max_size_mb"},
		{"upload.max_size_mb 负数", func(c *Config) { c.Upload.MaxSizeMB = -1 }, "upload.max_size_mb"},
		{"upload.max_size_mb 下界 1 通过", func(c *Config) { c.Upload.MaxSizeMB = 1 }, ""},
		{"upload.max_size_mb 上界 100 通过", func(c *Config) { c.Upload.MaxSizeMB = 100 }, ""},
		{"upload.max_size_mb 超上限", func(c *Config) { c.Upload.MaxSizeMB = 101 }, "upload.max_size_mb"},
		{"upload.oss.acl 留空跟随桶策略", func(c *Config) { c.Upload.OSS.ACL = "" }, ""},
		{"upload.oss.acl public-read 通过", func(c *Config) { c.Upload.OSS.ACL = "public-read" }, ""},
		{"upload.oss.acl private 通过", func(c *Config) { c.Upload.OSS.ACL = "private" }, ""},
		{"upload.oss.acl 非法值", func(c *Config) { c.Upload.OSS.ACL = "public" }, "upload.oss.acl"},
		{"upload.timeout 负数", func(c *Config) { c.Upload.Timeout = Duration(-time.Second) }, "upload.timeout"},

		{"recall_window 负数", func(c *Config) { c.Message.RecallWindow = Duration(-time.Second) }, "message.recall_window"},
		{"recall_window 为 0 交给默认值", func(c *Config) { c.Message.RecallWindow = 0 }, ""},

		// 多个问题同时存在时只报第一个，顺序即 validate 里的书写顺序。
		// 这决定运维看到的是哪条提示，改动判断顺序要意识到这一点。
		{"多错只报第一个", func(c *Config) {
			c.MySQL.DSN = ""
			c.Redis.Addr = ""
		}, "mysql.dsn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := baseConfig()
			if c.mutate != nil {
				c.mutate(cfg)
			}
			err := cfg.validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("报错信息应包含 %q，实际 %v", c.wantErr, err)
			}
		})
	}
}
