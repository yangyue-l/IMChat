# 部署说明

本文说明 IMChat 的部署形态、全部配置项、环境变量覆盖规则、多节点部署要点与优雅停机顺序。
架构取舍见 [architecture.md](./architecture.md)，接口契约见 [api.md](./api.md)。

---

## 一、部署形态与依赖

进程只有 `cmd/server` 一个入口，同一份二进制既能单机跑也能多节点跑，差别全在配置：

| 形态 | 关键配置 | 说明 |
|---|---|---|
| 单机 | `cluster.enabled=false` | 连接表、在线判定全在进程内，零跨节点开销 |
| 多节点 | `cluster.enabled=true` | 跨节点推送走 Redis Pub/Sub，在线状态写 Redis |

外部依赖：

| 依赖 | 用途 | 是否可省 |
|---|---|---|
| MySQL 8 | 消息、关系、动态等全部持久化数据 | 不可省，`mysql.dsn` 缺失启动即失败 |
| Redis 7 | 缓存、序列号、未读计数、已读、设备在线、限流窗口、跨节点总线 | 不可省，`redis.addr` 缺失启动即失败 |
| RabbitMQ 4 | 朋友圈写扩散、离线通知 | `rabbitmq.enabled=false` 时降级为进程内内存总线 |

本地依赖用 compose 拉起（端口做了错开，避免与本机已有服务冲突）：

```bash
docker compose -f deploy/docker-compose.yml up -d
```

启动服务，`-node` 是雪花 ID 的节点号：

```bash
go run ./cmd/server -c config/config.yaml -node 1
```

---

## 二、首次启动与结构变更

- 库本身由 `deploy/init/01-init.sql` 建（`utf8mb4` + `utf8mb4_unicode_ci`），compose 会把它挂进
  `docker-entrypoint-initdb.d` 自动执行。
- **业务表由 GORM `AutoMigrate` 创建**，开关是 `mysql.auto_migrate`（代码零值为 `false`，
  仓库自带配置写的是 `true`）。表结构的唯一事实来源是 `internal/model` 的 gorm tag，没有独立的建表清单。
- `AutoMigrate` 只会**加**，不会删也不会改：索引改列序、改唯一性、删索引这三类必须用
  `deploy/migrate/<日期>-*.sql` 补脚本，且顺序固定为「**先跑脚本、再重启服务**」，
  让 AutoMigrate 在同一轮启动里把新索引建回来。

```bash
mysql -h127.0.0.1 -P13306 -uroot -p imchat < deploy/migrate/20261024-index-refactor.sql
```

- 迁移脚本写成可重复执行（`information_schema` 判断 + `DROP`），重跑不会报错；
  但索引重建期间会缺一个分页索引，安排在低峰期。
- 生产建议把 `auto_migrate` 关掉，改为发布流水线里显式执行迁移，避免「重启即改结构」的隐式行为。

---

## 三、配置项总表

「默认值」是 `applyDefaults` 填的代码默认值，配置文件留空即取它。
**布尔开关没有默认值**：`enabled`、`pprof`、`auto_migrate`、`distributed` 不写就是 `false`，
这与「写了 `false`」等价，不存在「未填」这一状态。

### server

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `addr` | `:8080` | 非空，合法监听地址 | HTTP 与 WebSocket 共用同一个端口 |
| `mode` | `debug` | `debug` / `release` | `debug` 开 gin 调试输出；`release` 时日志走 JSON |
| `read_timeout` | `15s` | 正时长 | `http.Server.ReadTimeout` |
| `write_timeout` | `15s` | 正时长 | `http.Server.WriteTimeout`，**同时**用作请求级 context 超时 |
| `ws_ping_interval` | `30s` | 正时长 | 服务端心跳间隔 |
| `ws_pong_wait` | `75s` | 正时长，**必须大于** `ws_ping_interval` | 读超时，漏收 Pong 超过它即判连接死亡；配成小于等于心跳间隔会让连接建好即被判死，启动期直接报错 |
| `ws_write_wait` | `10s` | 正时长（> 0） | 单帧写超时，为 0 时每一帧（含心跳）都立刻超时 |
| `ws_send_buffer` | `512` | 正整数 | 单连接发送队列长度，满即断开该连接 |
| `graceful_timeout` | `15s` | 正时长 | 停机总预算，见第六节 |
| `pprof` | `false` | 布尔 | 打开才挂载 `/debug/pprof` |

### mysql

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `dsn` | 无 | 非空，**必填** | 需带 `charset=utf8mb4&parseTime=True&loc=Local` |
| `max_idle_conns` | `20` | 正整数 | 空闲连接数 |
| `max_open_conns` | `200` | 正整数 | 最大连接数，多节点时按 `节点数 × 该值` 压 MySQL |
| `conn_max_lifetime` | `30m` | 正时长 | 要短于 MySQL 的 `wait_timeout`，否则拿到已被服务端关掉的连接 |
| `conn_max_idle_time` | `5m` | 正时长 | 空闲连接回收 |
| `auto_migrate` | `false` | 布尔 | 启动时对齐表结构，见第二节 |

### redis

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `addr` | 无 | 非空，**必填** | `host:port` |
| `password` | 空 | 字符串 | 无密码留空 |
| `db` | `0` | 非负整数 | 多套环境共用一个 Redis 时用它隔离 |
| `pool_size` | `200` | 正整数 | 连接池上限 |
| `min_idle_conns` | `20` | 正整数 | 预热连接数 |
| `dial_timeout` | `2s` | 正时长 | 建连超时 |
| `read_timeout` | `1s` | 正时长 | 读超时 |
| `write_timeout` | `1s` | 正时长 | 写超时 |

### rabbitmq

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `url` | 空 | `amqp://` 开头 | 未填且 `enabled=false` 时不需要 |
| `enabled` | `false` | 布尔 | 关掉后降级为进程内内存总线，功能一致、不跨进程 |
| `prefetch` | `100` | 正整数 | 每个 channel 的未确认上限 |
| `publisher_confirm` | `false` | 布尔 | 开启后 `Publish` 等 broker 确认，延迟换可靠性 |
| `channel_pool` | `8` | 正整数 | 生产者复用的 channel 数 |
| `consumer_workers` | `3` | `0~32`，`0` 视为未填 | 每个 worker 独占一条 channel；未确认总量 = `prefetch × workers` |

### cluster

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `enabled` | `false` | 布尔 | 打开后跨节点投递，并在 Redis 写设备在线状态 |
| `shards` | `64` | **2 的幂** | 本地连接分片数，非 2 的幂启动即失败 |
| `bus_channel` | `im:bus` | 非空字符串 | 跨节点总线通道名，同一集群必须一致 |

### ratelimit

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `enabled` | `false` | 布尔 | 总开关 |
| `distributed` | `false` | 布尔 | 打开后登录/写接口叠加 Redis 滑动窗口，配额跨节点共享 |
| `http_qps` / `http_burst` | `50` / `100` | 正数 | 读接口：单用户每秒 |
| `write_qps` / `write_burst` | `10` / `20` | 正数 | 写接口瞬时速率：单用户每秒 |
| `write_limit` / `write_window` | `120` / `1m` | 正数 | 写接口窗口总量：单用户每窗口，跨节点共享 |
| `auth_qps` / `auth_burst` | `5` / `10` | 正数 | 登录注册瞬时速率：单 IP 每秒 |
| `auth_limit` / `auth_window` | `20` / `1m` | 正数 | 登录窗口总量：单 IP 每窗口，跨节点共享 |
| `upload_qps` / `upload_burst` | `2` / `5` | 正数 | 上传：单用户每秒 |
| `ws_handshake_qps` / `ws_handshake_burst` | `2` / `4` | 正数 | WS 握手：单 IP 每秒 |
| `ws_max_conns` | `5` | 正整数 | 单用户最大在线设备数，**按节点计** |
| `ws_msg_qps` / `ws_msg_burst` | `20` / `40` | 正数 | WS 上行：单连接每秒 |

`distributed=true` 时 `write_limit`/`write_window` 与 `auth_limit`/`auth_window` 必须为正数，
否则启动报错 —— 少了窗口长度等于没有配额，不能让某个接口上线后全程 429。

### cache / jwt / upload / message

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `cache.user_ttl` | `10m` | 正时长 | 用户资料缓存 |
| `cache.friend_ttl` | `5m` | 正时长 | 好友/拉黑关系缓存 |
| `cache.group_member_ttl` | `5m` | 正时长 | 群成员关系缓存 |
| `cache.ttl_jitter` | `1m` | 正时长 | 过期时间随机抖动，防雪崩 |
| `jwt.secret` | 无 | **至少 16 位**，必填 | 生产必须替换；多节点必须一致 |
| `jwt.expire` | `168h` | 正时长 | 令牌有效期 |
| `jwt.issuer` | `imchat` | 非空字符串 | 签发者，解析时校验 |
| `upload.dir` | `./uploads` | 可写目录 | 本地存储目录，同时也是 `/uploads` 静态路由的根 |
| `upload.max_size_mb` | `10` | `1`~`100` | 单文件上限。负数会被标准库当成 0 而上传播失败，过大则单请求就吃掉磁盘与带宽 |
| `upload.public_base` | 空 | 字符串 | 对外访问前缀，留空返回相对路径 |
| `message.recall_window` | `2m` | 非负时长 | 撤回自己的消息的时间窗，负数启动即报错 |

> 仓库自带的 `config/config.yaml` 与代码默认值有若干处不同（例如 `pprof: true`、
> `ratelimit.auth_limit: 30`、各类 `enabled` 打开）。**实际生效的是文件值**，
> 该文件是本地开发配置，生产请另起一份。

---

## 四、环境变量

只有**字符串**项支持环境变量覆盖，数字、布尔、时长只能改配置文件：

| 环境变量 | 覆盖项 |
|---|---|
| `IMCHAT_SERVER_ADDR` | `server.addr` |
| `IMCHAT_MYSQL_DSN` | `mysql.dsn` |
| `IMCHAT_REDIS_ADDR` | `redis.addr` |
| `IMCHAT_REDIS_PASSWORD` | `redis.password` |
| `IMCHAT_RABBITMQ_URL` | `rabbitmq.url` |
| `IMCHAT_JWT_SECRET` | `jwt.secret` |
| `IMCHAT_UPLOAD_PUBLIC_BASE` | `upload.public_base` |

加载顺序是 **YAML → 代码默认值 → 环境变量 → 校验**：

- 环境变量在默认值之后生效，所以「配置文件不写、只靠容器注入 DSN」的部署方式是成立的；
- **空字符串视为未设置**，不能用 `IMCHAT_REDIS_PASSWORD=""` 去主动清空配置里的密码；
- 校验在最后，注入的值同样要过 `jwt.secret ≥ 16 位`、`dsn` 非空这些门槛。

---

## 五、多节点要点

1. **`-node` 必须唯一**（`0~1023`）。雪花 ID 的节点号重复会让两个节点生成相同消息 ID，
   撞上 `uk_conv_seq` 或 `uk_from_client` 后表现为「偶发消息丢失/重复」，且没有任何启动期报错。
2. **`jwt.secret` 必须一致**。否则 A 节点签发的令牌在 B 节点验签失败，表现为随机 40100。
3. **`cluster.bus_channel` 必须一致**，且各节点连**同一个 Redis 实例的同一个 `db`**：
   总线是 Pub/Sub，通道名或库不同＝收不到对方的广播，跨节点推送静默失效。
4. **`cluster.shards` 是本地连接表的分片数**，各节点可以不同（分片只是本地散列落点），
   但都必须是 2 的幂；调它只影响本地锁竞争，不影响跨节点语义。
5. **单机模式下不写 `im:online:`**。这份状态只在 `cluster.enabled=true` 时维护，
   所以「列出在线设备」这类能力走的是 `Hub` 的两段式判定（本地分片为准、集群才回源 Redis）。
6. **限流配额口径随 `distributed` 变化**：`distributed=false` 时进程内令牌桶按节点各算一份，
   集群下实际配额 = 配置值 × 节点数；`distributed=true` 时段窗口总量跨节点共享，
   扩容不会顺带放宽配额，应优先用它。
7. **`ws_max_conns` 按节点计**：同一用户的两个设备连到不同节点时，各节点各自计数。
8. **`upload.dir` 是本地磁盘**。多节点必须挂共享存储，或前置对象存储并改 `upload.public_base`
   指向外部地址；否则 A 节点上传的图片在 B 节点 404。
9. **RabbitMQ 队列名固定**（`im.events` / `im.events.q`），多节点自然形成竞争消费，
   不需要额外配置；`consumer_workers × prefetch` 要按节点数折算总未确认量。
10. **`mysql.max_open_conns` 要折算**：默认 200 是单节点口径，10 个节点就是 2000 条连接。

---

## 六、优雅停机

进程监听 `SIGINT` / `SIGTERM`，收到后按下面顺序收尾（`cmd/server/main.go`）：

```
信号
 └─▶ server.Shutdown(ctx)      停止接收新请求，等在飞请求结束
      └─▶ hub.Stop(ctx)        关闭全部 WebSocket 连接，等读写协程退出
           └─▶ consumer.Close() 停止消费（未确认消息回队列）
                └─▶ producer.Close()  关闭生产者连接池
                     └─▶ rdb.Close()  释放 Redis 连接（defer）
```

要点：

- **三步共用 `graceful_timeout` 一个预算**。HTTP 阶段如果在飞请求拖满 15s，WS 阶段拿到的是
  同一个已过期的 context，会立刻放弃等待。所以该值应大于「最长请求耗时 + WS 收尾时间」，
  而不是各阶段各 15s。
- **`rabbitmq.enabled=false` 时**，第三步换成 `localBus.Close()`：内存队列里的残留事件直接丢弃，
  这是降级模式的已知代价。
- **强杀会丢消息**：`SIGKILL`（或 Kubernetes 的 `terminationGracePeriodSeconds` 短于
  `graceful_timeout`）跳过整个流程，未确认的 MQ 消息与在飞请求都不会被处理。
  容器编排里要把 `terminationGracePeriodSeconds` 设得比 `graceful_timeout` 更大。
- **客户端侧**：WS 被关闭后客户端按 `seq` 拉历史补齐即可，推送从不承担持久化责任。

---

## 七、健康检查与运维端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `/healthz` | 无 | 并发探测 MySQL 与 Redis（各 1s 超时），返回 `{"status":"ok\|degraded", "mysql":"ok\|down", "redis":"ok\|down"}` |
| `/metrics` | 无 | Prometheus 文本格式，见下 |
| `/debug/pprof/*` | 无 | 仅 `server.pprof=true` 时挂载，生产慎开 |
| `/uploads/*` | 无 | 静态文件，根目录是 `upload.dir` |
| `/ws` | 握手参数带令牌 | 浏览器 WS 不能设自定义头，令牌走查询参数 |

`/healthz` 的 **HTTP 状态码恒为 200**，是否摘机由负载均衡按 `status` 字段判断：
依赖抖动时返回 503 会让 LB 反复摘除/加回，比「降级但仍服务」伤害更大。
降级原因只写日志，不进响应 —— 该端点不鉴权，驱动错误里常带 DSN、端口、账号。

`/metrics` 指标：

| 指标 | 含义 |
|---|---|
| `imchat_online_connections` | 本节点当前在线连接数 |
| `imchat_frames_received_total` | 累计接收的上行帧数 |
| `imchat_frames_sent_total` | 累计写出的下行帧数 |
| `imchat_frames_dropped_total` | 因发送队列满而丢弃的帧数（等价于被判为慢客户端而断开的连接数） |
| `imchat_goroutines` | goroutine 数量 |
| `imchat_heap_alloc_bytes` | 堆内存占用 |
| `imchat_gc_count_total` | GC 次数 |

多节点部署时这些是**单节点**视角的值，监控侧按实例聚合，不要按集群求和后看绝对值。

---

## 八、启动期常见报错

这些错误都在 `config.validate` 或装配阶段抛出，进程直接退出，不会带病启动：

| 报错 | 原因 | 处理 |
|---|---|---|
| `mysql.dsn 不能为空` | 配置与环境变量都没给 | 填 `mysql.dsn` 或注入 `IMCHAT_MYSQL_DSN` |
| `redis.addr 不能为空` | 同上 | 填 `redis.addr` 或注入 `IMCHAT_REDIS_ADDR` |
| `jwt.secret 长度至少 16 位` | 用了过短的密钥 | 换 32 位以上随机串，多节点保持一致 |
| `server.addr 不能为空` | 绕过默认值构造出的空地址（`applyDefaults` 会填 `:8080`） | 填 `:8080` 或注入 `IMCHAT_SERVER_ADDR` |
| `server.ws_pong_wait(10s) 必须大于 ws_ping_interval(30s)` | 读超时配得不比心跳间隔长，第一条 Ping 还没发读截止就到了 | 保证前者大于后者，默认 `75s` / `30s` |
| `server.ws_write_wait 必须为正数，当前 0s` | 写截止时间等于「现在」，每一帧都立刻超时 | 填正时长，按网络状况取 `5s`~`10s` |
| `cluster.shards 必须是 2 的幂，当前 N` | 写了 48、100 这类值 | 改成 32 / 64 / 128 |
| `rabbitmq.consumer_workers 取值 0~32（0 表示未填，默认 3），当前 N` | 每个 worker 独占一条 channel，配几百个会打爆 broker | 按 CPU 与消息量取 3~8 |
| `ratelimit 启用 distributed 时 write 的 limit/window 必须为正数` | 开了 `distributed` 却把窗口或配额留成 0 | 两对 `limit`/`window` 都要填正数 |
| `upload.max_size_mb 取值 1~100，当前 N` | 负数会被 `http.MaxBytesReader` 当成 0 上限，所有上传都失败 | 按图片场景取 `5`~`20` |
| `message.recall_window 不能为负数，当前 …` | 笔误（`-2m`） | 改为正时长 |
| `配置时长 "30" 非法` | 时长漏了单位 | 写 `30s` / `1m` / `1h` |
| `初始化雪花节点失败` | `-node` 超出 `0~1023` | 改到合法区间，并保证集群内唯一 |

`consumer_workers` 的 `0` 表示「没填」，由 `applyDefaults` 归一成默认的 3，
只有负数与大于 32 才会被拦下 —— 报错文案按这个真实范围写。
