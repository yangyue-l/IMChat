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

本地依赖用 compose 拉起（全部用默认端口；本机已有 MySQL / Redis 时会端口冲突，
需要先停掉它们，或改端口映射并同步 `config/config.yaml`）：

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
mysql -h127.0.0.1 -P3306 -uroot -p imchat < deploy/migrate/20261024-index-refactor.sql
```

- 迁移脚本写成可重复执行（`information_schema` 判断 + `DROP`），重跑不会报错；
  但索引重建期间会缺一个分页索引，安排在低峰期。
- 生产建议把 `auto_migrate` 关掉，改为发布流水线里显式执行迁移，避免「重启即改结构」的隐式行为。

---

## 三、配置项总表

「默认值」是 `applyDefaults` 填的代码默认值，配置文件留空即取它。
**布尔开关没有默认值**：`enabled`、`pprof`、`auto_migrate`、`distributed` 不写就是 `false`，
这与「写了 `false`」等价，不存在「未填」这一状态。

`server.metrics` 与 `server.healthz` 是两个例外：**不写视为开启**。这两个端点默认必须暴露，
否则老配置升级后会因为缺字段静默失去指标、或让探针拿到 404 把健康实例整批摘掉；
字段声明成 `*bool` 正是为了区分「没写」与「写了 false」。

### server

| 配置项 | 默认值 | 取值范围 | 说明 |
|---|---|---|---|
| `addr` | `:8080` | 非空，合法监听地址 | HTTP 与 WebSocket 共用同一个端口 |
| `mode` | `debug` | `debug` / `release` | `debug` 开 gin 调试输出；`release` 时日志走 JSON |
| `read_timeout` | `15s` | 正时长 | `http.Server.ReadTimeout`，覆盖读请求体的全过程；上传路由会在 handler 里按 `upload.timeout` 重设连接 deadline |
| `write_timeout` | `15s` | 正时长 | `http.Server.WriteTimeout`，**同时**用作普通接口的请求级 context 超时；`/upload` 用 `upload.timeout` 另算，`/ws` 不设（长连接） |
| `ws_ping_interval` | `30s` | 正时长 | 服务端心跳间隔 |
| `ws_pong_wait` | `75s` | 正时长，**必须大于** `ws_ping_interval` | 读超时，漏收 Pong 超过它即判连接死亡；配成小于等于心跳间隔会让连接建好即被判死，启动期直接报错 |
| `ws_write_wait` | `10s` | 正时长（> 0） | 单帧写超时，为 0 时每一帧（含心跳）都立刻超时 |
| `ws_send_buffer` | `512` | 正整数 | 单连接发送队列长度，满即断开该连接 |
| `graceful_timeout` | `15s` | 正时长 | 停机总预算，见第六节 |
| `pprof` | `false` | 布尔 | 打开才挂载 `/debug/pprof` |
| `metrics` | **`true`** | 布尔 | 不写即暴露 `/metrics`；显式写 `false` 才关，关掉后该路径返回 404 |
| `healthz` | **`true`** | 布尔 | 不写即挂载 `/healthz`；显式写 `false` 才关，关掉后该路径返回 404。关闭时 `Dockerfile` 的 `HEALTHCHECK` 与负载均衡探针必须一起停掉，否则实例恒为不健康 |

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
| `upload.max_size_mb` | `10` | `1`~`100` | 单文件上限。负数会被标准库当成 0 而上传播失败，过大则单请求就吃满内存与带宽 |
| `upload.timeout` | `120s` | 正时长 | 单次上传的请求预算，与 `server.write_timeout` 分开。10MB 在 1.4Mbps 上要 60s，用普通接口的 15s 会让弱网用户稳定上传失败 |
| `upload.public_base` | 空 | `http(s)://` 开头的绝对地址 | 图片对外前缀。留空按 `https://{bucket}.{endpoint}` 推导；写成裸域名会被前端当成相对路径，图片静默 404 |
| `upload.oss.endpoint` | 无 | 必填 | bucket 所在区域的访问域名，不带 bucket 前缀，协议头可省略（缺省按 https） |
| `upload.oss.bucket` | 无 | 必填 | 桶名 |
| `upload.oss.acl` | 空 | `private` / `public-read` / `public-read-write` / `default` | 写入对象的 ACL。留空表示不设，跟随桶的默认策略；**桶是私有的、图片又要按 URL 直读时必须填 `public-read`**，否则上传成功但地址是 403 |
| `upload.oss.access_key_id` / `access_key_secret` | 无 | 必填 | RAM 用户凭据，最小权限只需 `oss:PutObject` / `oss:GetObject`；**不要写进 yaml**，走环境变量 |
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
| `IMCHAT_OSS_ENDPOINT` | `upload.oss.endpoint` |
| `IMCHAT_OSS_BUCKET` | `upload.oss.bucket` |
| `IMCHAT_OSS_ACCESS_KEY_ID` | `upload.oss.access_key_id` |
| `IMCHAT_OSS_ACCESS_KEY_SECRET` | `upload.oss.access_key_secret` |
| `IMCHAT_OSS_ACL` | `upload.oss.acl` |

加载顺序是 **`.env` → YAML → 代码默认值 → 环境变量 → 派生值 → 校验**：

- `.env` 不是新增的一层优先级，它只是「进程环境变量的一个文件来源」：解析时**已存在的键一律跳过**，
  所以容器 / CI 注入的值永远压过 `.env`，部署形态完全不受影响；
- 文件位置固定为**当前工作目录下的 `.env`**（不是 yaml 所在目录，也没有命令行参数可改），
  找不到就静默跳过，生产镜像里不需要它；
- 模板是仓库根目录的 `.env.example`，复制成 `.env` 后填写即可，`.env` 已在 `.gitignore` 里；
- 只认上面表格里的 12 个键，连接池、超时、限流参数写进 `.env` 不会生效，仍然要改 yaml；
- `.env` 里写成 `KEY=` 等价于没写，同样不能主动清空 yaml 里的值；
- 解析失败（缺 `=`、键名为空）会让启动直接失败，不会静默跳过整行 —— 密钥拼错却照常启动，
  最后表现为上传 403，排查成本太高；代价是报错由 godotenv 给出，不带行号；
- 解析交给 godotenv，它比「纯 `KEY=VALUE`」多几条语法：`export` 前缀、`${VAR}` 展开、
  `FOO: bar` 这种 YAML 写法，以及**行内注释** —— 值里出现「空白 + `#`」之后会被截掉。
  密钥含 `#` 必须用引号包住，否则会被悄悄截断；
- `loadDotEnv` 自己剥了 UTF-8 BOM：Windows 记事本保存过的 `.env` 会让首行键名带上隐形字符，
  godotenv 不管这个，不剥就永远匹配不上，而且没有任何报错。
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
8. **图片全部在 OSS**，多节点天然共享同一份数据，不需要挂共享磁盘，也没有「A 节点上传、B 节点 404」的问题。
   库里存的是对象 key，对外地址由 `media` 拼成：**换 CDN 只改 `upload.public_base`，
   不用刷 `users` / `groups` / `moments` / `messages` 四张表**。唯一会短暂滞留旧域名的是
   用户信息缓存（`cache.user_ttl`，默认 10 分钟），切域名后等一个 TTL 或重启即可。
   对象 key 不可变，所以上传时给了 `Cache-Control: public, max-age=31536000, immutable`：
   换头像/改图生成的是新 key，地址随之变化，不会出现「改了图还看到旧的」。
9. **回收是尽力而为**：换头像、删动态、撤回图片会顺手删掉旧对象，但只删「库里那个 key
   确实属于本次操作者」的对象（key 的第一段是上传者 uid）。管理员换掉另一位管理员传的群头像时
   会跳过，留下一个孤儿对象 —— 这是有意的取舍，宁可不删也不按库里存的地址删别人的图。
   孤儿不影响功能，只占存储；需要彻底清干净时按日志里的「回收对象失败」人工补删。
10. **RabbitMQ 队列名固定**（`im.events` / `im.events.q`），多节点自然形成竞争消费，
    不需要额外配置；`consumer_workers × prefetch` 要按节点数折算总未确认量。
11. **`mysql.max_open_conns` 要折算**：默认 200 是单节点口径，10 个节点就是 2000 条连接。

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
| `/healthz` | 无 | 并发探测 MySQL、Redis 与 OSS（各 1s 超时），返回 `{"status":"ok\|degraded", "mysql":"ok\|down", "redis":"ok\|down", "oss":"ok\|down"}`；`server.healthz=false` 时整条路由不注册 |
| `/metrics` | 无 | Prometheus 文本格式，见下；`server.metrics=false` 时整条路由不注册 |
| `/debug/pprof/*` | 无 | 仅 `server.pprof=true` 时挂载，生产慎开 |
| `/ws` | 握手参数带令牌 | 浏览器 WS 不能设自定义头，令牌走查询参数 |

图片不走本服务：读取由 CDN 或 bucket 域名直连，后端没有任何静态文件路由，
也不托管前端页面（前端产物由 nginx 或静态站点托管）。

`/healthz` 的 **HTTP 状态码恒为 200**，是否摘机由负载均衡按 `status` 字段判断：
依赖抖动时返回 503 会让 LB 反复摘除/加回，比「降级但仍服务」伤害更大。
降级原因只写日志，不进响应 —— 该端点不鉴权，驱动错误里常带 DSN、端口、账号。

`/metrics` 指标（`_total` 结尾的累计值声明为 `counter`，其余为 `gauge`）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `imchat_online_connections` | gauge | 本节点当前在线连接数 |
| `imchat_frames_received_total` | counter | 累计接收的上行帧数 |
| `imchat_frames_sent_total` | counter | 累计写出的下行帧数 |
| `imchat_frames_dropped_total` | counter | 因发送队列满而丢弃的帧数（等价于被判为慢客户端而断开的连接数） |
| `imchat_goroutines` | gauge | goroutine 数量 |
| `imchat_heap_alloc_bytes` | gauge | 堆内存占用 |
| `imchat_gc_count_total` | counter | GC 次数 |
| `imchat_upload_success_total` | counter | 累计写入对象存储成功的图片数 |
| `imchat_upload_failed_total` | counter | 累计写入对象存储失败的图片数（参数校验失败不计入） |
| `imchat_upload_bytes_total` | counter | 累计写入对象存储的图片字节数 |

上传三个指标只数「真正进了存储层」的那一步：格式、大小这类参数校验失败不算，
否则用户乱传文件时失败率会飙升，把真正的存储故障淹掉。

关掉用 `server.metrics: false`，路由不注册、请求直接 404 —— 返回 404 而不是空响应，
是为了把「配错了」和「抓到了空数据」区分开。这个端点与 `/healthz` 一样不鉴权，
公网暴露等于把 goroutine 数、堆内存、上传总量这些内部状态交出去，入口处要么按
路径拦截，要么显式关闭。

`/healthz` 同样可以用 `server.healthz: false` 关掉（不写则默认开启，与 `metrics` 一致）。
它没有后台协程也没有定时器，只在请求到达时才执行一次探测，所以常驻成本是零；
关掉纯粹是为了本机开发时不暴露运维端点。**注意关掉之后容器探针会拿到 404**，
`Dockerfile` 的 `HEALTHCHECK` 与负载均衡的健康检查必须同步停掉，否则实例恒为不健康。

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
| `upload.timeout 必须为正数，当前 …` | 配成 `0s` 或负数，上传请求一开始就超时 | 填正时长，弱网场景不低于 `60s` |
| `upload.oss.acl 只能是 private / public-read / …，当前 …` | ACL 写成了 `public`、`PublicRead` 这类非 OSS 枚举值 | 用 OSS 的四种取值之一，或留空跟随桶策略 |
| `message.recall_window 不能为负数，当前 …` | 笔误（`-2m`） | 改为正时长 |
| `配置时长 "30" 非法` | 时长漏了单位 | 写 `30s` / `1m` / `1h` |
| `初始化雪花节点失败` | `-node` 超出 `0~1023` | 改到合法区间，并保证集群内唯一 |

`consumer_workers` 的 `0` 表示「没填」，由 `applyDefaults` 归一成默认的 3，
只有负数与大于 32 才会被拦下 —— 报错文案按这个真实范围写。

---

## 九、容器化

根目录的 `Dockerfile` 是多阶段构建：`golang:1.26-alpine` 编出 `CGO_ENABLED=0` 的静态二进制，
运行层用 `alpine`，装了 `ca-certificates`（调 OSS 的 HTTPS 需要根证书）与 `tzdata`
（DSN 用 `loc=Local`，缺了时间偏 8 小时），并以 uid 10001 的非 root 用户运行，产物约 62MB。

```bash
docker build -t imchat:latest .
```

```bash
docker run -d --name imchat -p 8080:8080 --stop-timeout 30 \
  -e IMCHAT_MYSQL_DSN='root:root123456@tcp(mysql:3306)/imchat?charset=utf8mb4&parseTime=True&loc=Local' \
  -e IMCHAT_REDIS_ADDR=redis:6379 \
  -e IMCHAT_RABBITMQ_URL='amqp://imchat:imchat123456@rabbitmq:5672/' \
  -e IMCHAT_OSS_BUCKET=your-bucket \
  -e IMCHAT_OSS_ACCESS_KEY_ID=... \
  -e IMCHAT_OSS_ACCESS_KEY_SECRET=... \
  imchat:latest
```

四个必踩的点：

1. **镜像里的 `config.yaml` 是本地开发配置**（`127.0.0.1:3306`）。容器里必须用
   `IMCHAT_MYSQL_DSN` / `IMCHAT_REDIS_ADDR` / `IMCHAT_RABBITMQ_URL` 覆盖成服务名，
   否则会连到容器自己的回环地址，报 `connection refused`。
2. **`.env` 不进镜像**（`.dockerignore` 已排除）。凭据走 `-e` 或 `--env-file`，
   后者的格式与 `.env` 相同，可以直接复用同一个文件。
3. **优雅停机**：`graceful_timeout` 默认 `15s`，而 Docker 默认在 `SIGTERM` 后 10s 就发
   `SIGKILL`。要显式给 `--stop-timeout 30`（compose 里是 `stop_grace_period: 30s`），
   否则连接会被硬切，客户端看到的是「连接异常断开」而不是正常关闭。
4. **`HEALTHCHECK` 探的是 `/healthz`，它永远返回 200**：只反映进程还能响应 HTTP，
   抓的是死锁、goroutine 泄漏这类假活；依赖可用性在响应体里，DB 挂了不会变成 unhealthy
   —— 那种情况重启容器也解决不了。注意 `server.healthz=false` 会让这条探针拿到 404
   而恒定判为 unhealthy，两者必须一起改。

构建阶段在容器内跑 `go mod download`，没有本机的 `GOPROXY` 配置，所以 Dockerfile 默认给了
`goproxy.cn`；在海外构建时用 `--build-arg GOPROXY=` 覆盖成空即可走官方源。
