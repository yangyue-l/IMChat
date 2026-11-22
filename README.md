# IMChat · 即时通讯系统

一套可运行、可扩展的 IM 后端 + 演示前端，覆盖私聊、群聊、好友、拉黑、朋友圈全套功能，
面向**高并发 / 高可用 / 高性能**场景做了针对性设计。

技术栈：**Go 1.26.4+ · Gin · GORM · MySQL 8 · Redis 7 · RabbitMQ 4 · WebSocket**

---

## 一、功能清单

| 模块 | 能力 |
|---|---|
| 账号 | 注册、登录、登出（令牌黑名单）、资料查看与修改、用户搜索 |
| 好友 | 发起申请、同意/拒绝、列表、备注、星标、删除好友 |
| 黑名单 | 拉黑、解除、列表；拉黑后双向禁止消息与朋友圈互见 |
| 私聊 | 文本/图片/文件消息、历史消息游标分页、已读回执、未读数、撤回（2 分钟） |
| 群聊 | 建群、邀请、退群、踢人、禁言、设管理员、转让群主、群公告、解散群 |
| 会话 | 会话列表、最后一条消息预览、未读角标、置顶、免打扰 |
| 朋友圈 | 发布（图文/可见范围）、好友时间线、点赞、评论、删除、他人主页 |
| 实时 | WebSocket 推送、心跳保活、多端在线、正在输入、断线重连 |

---

## 二、快速开始

### 1. 启动依赖

```bash
docker compose -f deploy/docker-compose.yml up -d
```

> 依赖容器用默认端口：MySQL `3306`、Redis `6379`、RabbitMQ `5672`、管理台 `15672`。
> 本机已有 MySQL / Redis 时会端口冲突，先停掉它们；或者改 `deploy/docker-compose.yml`
> 的端口映射，并同步修改 `config/config.yaml`。

### 2. 启动服务

```bash
go run ./cmd/server -c config/config.yaml -node 1
```

服务启动后会自动建表。后端只提供 API 与 WebSocket，不托管页面 ——
验证接口用第 4 节的端到端冒烟，或者启动前端（见第 3 步）。

### 3. 前端

后端不托管前端：gin 只出 `/api/v1` 与 `/ws`，页面产物自行交给 nginx 或静态站点。

```bash
make web       # 构建产物到 web/index.html
make web-dev   # 开发服务器起在 :5173，/api 与 /ws 代理到 :8080
```

仓库里已经提交了构建产物，只跑后端时无需安装 Node；改完前端源码再执行一次 `make web` 即可。

### 4. 验证（单节点 45 项，双节点 48 项）

```bash
# 单节点
go run ./test/e2e -base http://127.0.0.1:8080

# 双节点集群：额外验证跨节点推送（多 3 项断言）
go run ./cmd/server -c config/config-cluster.yaml -node 2 &
go run ./test/e2e -base http://127.0.0.1:8080 -cluster-base http://127.0.0.1:8081
```

覆盖：注册登录、好友申请与通过、私聊实时推送、消息幂等、撤回权限、
拉黑拦截、群聊权限矩阵、朋友圈可见性与拉黑过滤、已读回执、跨节点投递。

### 5. 关键地址

| 地址 | 用途 |
|---|---|
| `http://127.0.0.1:8080/healthz` | 健康检查（`server.healthz: false` 可关） |
| `http://127.0.0.1:8080/metrics` | Prometheus 指标（`server.metrics: false` 可关） |
| `http://127.0.0.1:8080/debug/pprof/` | 性能分析（`server.pprof: true` 才挂载） |
| `http://127.0.0.1:15672/` | RabbitMQ 管理台（imchat / imchat123456） |

---

## 三、目录结构

```
IMChat/
├── cmd/
│   └── server/            # 入口：装配依赖、优雅启停
├── config/                # YAML 配置（cluster 版用于双节点验证）
├── deploy/                # docker-compose 与初始化 SQL
├── internal/
│   ├── config/            # 配置加载（支持环境变量覆盖）
│   ├── model/             # GORM 模型
│   ├── repository/        # 数据访问
│   ├── service/           # 业务逻辑（核心）
│   ├── handler/           # HTTP 接口
│   ├── ws/                # WebSocket 网关（分片 Hub）
│   ├── mq/                # RabbitMQ 生产/消费 + 本地兜底总线
│   ├── cache/             # Redis 封装（缓存/计数/在线状态）
│   ├── infra/             # MySQL / Redis 连接
│   ├── storage/           # 对象存储（阿里云 OSS），只管写入与回收
│   ├── middleware/        # 鉴权、限流、日志、跨域
│   └── pkg/               # 工具：key/URL 互转、jwt、雪花 ID、响应、日志
├── test/
│   └── e2e/               # 端到端冒烟测试：真 HTTP + WS，单节点 45 项
├── web/                   # 前端源码与构建产物（启动与构建见第二节）
└── docs/                  # 设计与接口文档
    ├── architecture.md    # 架构与关键取舍
    ├── data-model.md      # 数据模型字段与索引清单
    ├── redis-keys.md      # Redis Key 的结构、TTL 与使用场景
    ├── deploy.md          # 配置项总表、环境变量与多节点部署要点
    ├── error-codes.md     # 业务错误码与客户端分流口径
    └── api.md             # 接口说明
```

---

## 四、核心设计

### 4.1 双通道消息模型

- **发消息走 HTTP**：有明确的同步回执，便于重试与错误处理。
- **收消息走 WebSocket**：服务端推送，低延迟。
- WS 通道同时支持 `send` 帧，两种方式复用同一套 service，行为一致。

### 4.2 消息可靠性（标准版）

```
1. 幂等判定      client_msg_id -> Redis SETNX，重复请求直接返回已落库消息
2. 分配序号      Redis INCR 按会话自增，保证会话内严格有序
3. 落库          MySQL 唯一索引 client_msg_id 兜底并发重复
4. 会话/未读     私聊双方各写一行 + Redis 计数；群聊只更新群表指针（读扩散）
5. 在线推送      只序列化一次，多连接复用同一份字节
6. 离线补偿      全部接收者离线时投递 MQ，由消费者做离线通知
```

### 4.3 会话 ID 设计

| 类型 | 格式 | 示例 |
|---|---|---|
| 私聊 | `p_<小uid>_<大uid>` | `p_1001_1002` |
| 群聊 | `g_<gid>` | `g_2001` |

好处：会话 ID 天然唯一且可反解，消息表只需一张，无需按类型分表。

### 4.4 三高优化清单

**接入层**

- 连接按 `uid` 哈希分 64 片，每片独立锁，锁竞争降到 1/64。
- 读、写各一个 goroutine，心跳 ticker 复用写协程，不额外占 goroutine。
- 广播只序列化一次，所有连接共享同一份 `[]byte`。
- 发送队列非阻塞入队，队列满直接断开慢消费者，防内存雪崩。
- 跨节点投递通过 Redis Pub/Sub，按分片批量加锁，`cluster.enabled` 一键切换单机/集群。

**数据层**

- 关闭 GORM 默认事务包装、开启预编译语句缓存。
- 连接池参数显式配置，`ConnMaxLifetime` 短于 MySQL `wait_timeout`。
- 历史消息用 `seq` 游标分页，没有 `OFFSET` 深翻页。
- 会话列表：私聊写扩散（固定 2 行），群聊读扩散（只更新群表一行的最新消息指针，
  未读数用 seq 差值算），写入代价与群成员数解耦。
- 群人数、点赞数、评论数用冗余计数 + 原子增减，避免聚合查询。
- 多表写入一律走事务（消息 + 会话指针、成员 + 人数、点赞 + 计数），
  上限类校验在事务内 `SELECT ... FOR UPDATE`，杜绝并发越界。

**缓存层**

- `singleflight` 合并同 Key 并发回源，防击穿。
- 空值占位缓存，防穿透。
- 过期时间加随机抖动，防雪崩。
- 批量未读自增与续期同一条 Lua，N 次写压成一次原子执行。
- 好友/拉黑/群成员关系缓存，热点判断不落库。
- 已读上报、未读自增、分布式限流、设备在线、时间线写入用 Lua 脚本保证原子，
  详见 `docs/architecture.md` 第六之二节。
- 全部 Key 的结构、TTL、使用场景见 `docs/redis-keys.md`。

**异步层**

- RabbitMQ publisher confirm + 手动 ACK，消息不丢。
- 重试通过重投并累加 `x-retry`（`Nack requeue` 不会改 header，这点容易踩坑），
  同一条消息最多投递 3 次（含首次），仍失败转死信队列。
- MQ 未启用时自动降级为本地内存总线，同一套 handler 两种模式行为一致。

**接口层**

- 按用户维度令牌桶限流，本地实现零网络开销，定时清理闲置桶。
- JWT 无状态校验 + Redis 登出黑名单。
- 业务码统一出口 `internal/pkg/resp`，各码的触发场景与客户端分流口径见 `docs/error-codes.md`。
- 全链路 `context` 超时，`http.Server` 各阶段超时显式设置。
- 优雅停机：先停 HTTP，再断 WS，再停消费，最后释放连接。

---

## 五、配置说明

`config/config.yaml` 里的值都是本地开发可直接用的默认配置，只有 OSS 的桶名与密钥必须自己提供。
本地把它们写进项目根目录的 `.env`（模板见 `.env.example`），容器部署走环境变量注入，
两种方式的优先级都高于 yaml。最常改的几项：

| 环境变量 | 对应配置 |
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

加载顺序是 `.env` → YAML → 代码默认值 → 环境变量 → 派生值 → 校验；
只有字符串项能被环境变量覆盖，数字、布尔、时长只能改配置文件。
完整配置项（默认值、取值范围）、多节点部署要点与优雅停机顺序见 `docs/deploy.md`。

生产环境务必修改 `jwt.secret`（至少 16 位），多节点部署时各节点必须一致。

---

## 六、验证与压测

```bash
go run ./test/e2e -base http://127.0.0.1:8080   # 功能回归（单节点 45 项）
curl http://127.0.0.1:8080/metrics               # 在线连接、帧数、goroutine、堆内存
go tool pprof http://127.0.0.1:8080/debug/pprof/profile?seconds=30
```

建议关注：`imchat_online_connections`、`imchat_frames_dropped_total`（慢客户端导致丢帧）、
`imchat_goroutines`、GC 次数、MySQL 慢查询日志（已开启，阈值 200ms）。

---

## 七、后续演进方向

1. **消息表分库分表**：按 `conv_id` 哈希分片，或按时间分区。
2. **群列表上量**：用户加入的群过万时，把群列表与最新消息缓存进 Redis。
3. **推送网关**：离线通知接入 APNs / FCM / 厂商通道。
4. **敏感词与内容审核**：接入第三方审核服务，落库前拦截。
5. **可观测性**：接入 OpenTelemetry，链路追踪补全 MQ 段。

