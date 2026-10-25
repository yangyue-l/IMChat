# 架构与性能设计

本文说明 IMChat 的整体结构、关键链路，以及为「高并发 / 高可用 / 高性能」所做的具体取舍。

---

## 一、整体结构

```
                       ┌──────────────────────────┐
   浏览器 / 客户端  ──▶ │  HTTP API (Gin)          │
                       │  · JWT 鉴权              │
                       │  · 分级限流              │
                       │  · 统一响应与异常兜底    │
                       └────────────┬─────────────┘
                                    │
                       ┌────────────▼─────────────┐
                       │  WebSocket 网关           │
                       │  · 64 分片连接表          │
                       │  · 心跳 / 背压 / 顶号     │
                       │  · Redis 总线跨节点广播   │
                       └────────────┬─────────────┘
                                    │
                       ┌────────────▼─────────────┐
                       │  Service 业务层           │
                       │  消息 / 好友 / 群 / 朋友圈│
                       └───┬────────┬─────────┬────┘
                           │        │         │
              ┌────────────▼──┐ ┌───▼─────┐ ┌▼──────────────┐
              │ MySQL (GORM)  │ │ Redis   │ │ RabbitMQ      │
              │ 消息/关系/动态 │ │ 缓存/计数│ │ 异步扇出/通知 │
              └───────────────┘ └─────────┘ └───────────────┘
```

分层职责：

| 层 | 职责 | 不允许做的事 |
|---|---|---|
| handler | 参数解析、调用 service、组装响应 | 写业务规则、直接查库 |
| service | 全部业务规则、事务边界、缓存策略 | 依赖 gin / 直接读写 HTTP |
| repository | 单表 CRUD 与查询构造 | 跨表业务判断 |
| ws | 连接生命周期、帧编解码、推送 | 业务校验（转发给 service） |
| cache | Key 规划、缓存三大问题防护 | 业务语义 |

`service` 通过 `Pusher` / `Publisher` 两个接口依赖上层能力，因此 service 不 import ws、不 import mq，避免循环依赖。

---

## 二、核心链路：发一条消息

```
客户端
  │ POST /messages/private (client_msg_id)
  ▼
handler ──▶ service.SendPrivate
              │
              ├─1 关系校验：EitherBlocked(缓存) → AreFriends(缓存)
              │
              ├─2 幂等：Redis SETNX im:dedup:<from>:<client_msg_id>
              │      重复 → 回查 MySQL 返回原消息，直接结束
              │
              ├─3 序号：Redis INCR im:seq:<conv_id>
              │
              ├─4 落库：INSERT messages
              │      uk(from_id, client_msg_id) 兜住并发重复
              │
              ├─5 会话：UpsertConversations(批量 ON DUPLICATE KEY UPDATE)
              │      Redis Lua HINCRBY 未读数 + 续期
              │
              ├─6 推送：序列化一次 → Hub.PushToUsers
              │      按 uid 哈希分片 → 每片一次 RLock → 非阻塞入队
              │
              └─7 离线补偿：本地未投出 且 集群无人在线 → RabbitMQ 事件
  │
  ▼
返回完整消息（含 server id 与 seq）
```

关键点：

- **先落库再推送**，客户端拿到 `ack` 即代表消息持久化成功。
- **推送失败不影响写入**，客户端重连后用 `seq` 拉历史补齐。
- **序号由 Redis 分配**，单线程 INCR 天然有序且无锁竞争；MySQL 唯一索引保证不重。

---

## 三、WebSocket 网关设计

### 3.1 为什么要分片

朴素实现用一个全局 `map[int64]*Client` + 一把大锁，N 万连接下推送会串行等待。
这里按 `uid` 做哈希散列到 64 个 shard，每个 shard 独立 `sync.RWMutex`：

```go
x := uint64(uid)
x ^= x >> 33; x *= 0xff51afd7ed558ccd; x ^= x >> 33
shard := shards[x & 63]
```

批量推送时先按分片分组，加锁次数从 `O(用户数)` 降到 `O(分片数)`。

### 3.2 goroutine 模型

每个连接固定 **2 个** goroutine：

| goroutine | 职责 |
|---|---|
| readLoop | 读帧、限流、分发；`PongHandler` 续期读超时 |
| writeLoop | 写帧 + 心跳 ticker（复用同一个 select，不额外开协程） |

心跳不用「每连接一个 timer + 一个协程」，而是在写协程的 select 里挂 ticker，
10 万连接就是 20 万 goroutine，而不是 30 万。

### 3.3 背压与自我保护

- 每连接发送队列默认 512，非阻塞入队。
- 队列满说明客户端消费不过来，**直接断开**，防止内存被慢连接拖爆。
- 单帧上限 8KB，超出即断。
- 单连接令牌桶限流（默认 20 条/秒），挡住刷屏；单用户最多 5 个在线设备，
  握手阶段判定，超出直接 429 —— 否则多开连接就能变相放大上行配额。

### 3.4 集群模式

`cluster.enabled=true` 时：

1. 连接建立 / 心跳时把 `uid+device` 写进 `im:online:{uid}`（**zset**，score 是该设备自己的过期时刻，
   按设备独立过期，不用整 key TTL —— 否则一台设备心跳就给已断开的设备无限续命）。
   续期每 2 次心跳一次（30s × 2 = 60s），TTL 取 `2 个续期间隔 + 1 个心跳间隔`（150s），
   容得下**一次**续期失败；连接退出只清理自己那一份，被顶号的旧连接不会误删新连接的记录。
   单机模式不写这份状态（本地分片就是权威），所以判定入口只有一个：`Hub.Online(uids)`
   （实现 `service.Presence`）。它先扫本地分片，命中即在线；只有「本地未命中 + 开了集群」
   才把剩下的人一次性 pipeline 回源 `cache.OnlineSet`，好友列表、群消息扇出共用这一条路径，
   N 个人也只多一次往返。回源失败会返回 error 而不是「全都不在线」——两者必须能区分。
2. 推送先本地投递，再 `PUBLISH im:bus`，消息带 `origin` 节点标识。
3. 各节点订阅总线，收到后跳过自己发的，只推本节点连接。

代价：每条广播多一次 Redis 往返。因此**单机部署请保持 `cluster.enabled=false`**。

---

## 四、数据模型要点

### 4.1 统一消息表

私聊 `p_a_b`、群聊 `g_gid` 共表存储，避免两套查询逻辑。

索引：

| 索引 | 用途 |
|---|---|
| `uk_from_client (client_msg_id)` | 幂等兜底（单列全局唯一，不区分发送者） |
| `uk_conv_seq (conv_id, seq)` | 历史消息游标分页，同时兜住 seq 唯一（唯一索引） |

完整字段与索引清单见 [data-model.md](./data-model.md)。

### 4.2 会话列表：私聊写扩散，群聊读扩散

这是整套系统最关键的一处取舍，直接决定了大群场景能不能扛住。

**私聊走写扩散**（成员恒为 2 人）：

`conversations` 表双方各存一行，冗余 `last_msg_preview / last_msg_at`，
列表页零回表，一条 `ORDER BY pinned DESC, last_msg_at DESC LIMIT 200` 搞定，
未读数走 Redis 计数器。写入代价固定为 2 行，读取零计算。

**群聊走读扩散**（成员最多 1000 人）：

不为成员预建会话行，只更新 `groups` 表一行的最新消息指针
（`last_msg_id / last_msg_seq / last_msg_preview / last_msg_type / last_msg_at`），
成员读取会话列表时，用「我的群列表 + 群表的最新消息 + 已读序号」现场组装，
未读数 = `群最新 seq − 我的已读 seq`。

```
写扩散（旧）                          读扩散（现）
一条群消息                             一条群消息
 ├─ messages       INSERT 1 行          ├─ messages       INSERT 1 行
 ├─ conversations  UPSERT N 行  ← O(N)  └─ groups         UPDATE 1 行  ← O(1)
 └─ Redis          HINCRBY N 次 ← O(N)
```

实测（1000 人群，开启 MySQL general_log 统计）：

| 指标 | 数值 |
|---|---|
| 单条群消息 SQL 执行次数 | 5（INSERT messages 1 + UPDATE groups 1 + 校验查询 3） |
| `conversations` 写入 | **0** |
| Redis 未读计数写入 | **0** |
| 端到端耗时 | ~26ms |

同样的消息若走写扩散，将额外产生 1000 行 upsert 与 1000 次 `HINCRBY`。
大群高频发言时这部分写放大会直接打满 MySQL 与 Redis。

**代价与兜底**：

- 读取会话列表需要多一次 `group_members` 关联查询（走 `idx_user_joined` 索引）。
  用户通常只有几十个群，成本远低于写扩散。
- 读扩散下未读靠差值算，因此**新成员入群必须把已读位置对齐到群当前最新序号**，
  否则入群前的历史消息会被一次性算成未读。实现上用 `HSETNX`（不覆盖老成员）
  + `GREATEST`（只前进不后退）双重保护。
- 已读记录 Redis 未命中时回源 `conversation_reads`，避免 Redis 重启后未读计算失准。
- 从未上报过已读的会话按「从当前位置开始」处理，并异步补齐记录。

### 4.3 已读与未读

- **已读位置**存 `conversation_reads`（强一致，供多端同步）+ Redis Hash（快路径）。
- **私聊未读**存 Redis Hash 计数器（写扩散，+1 即可）。
- **群聊未读**不落存储，读取时用 `群最新 seq − 已读 seq` 实时计算。
- 进入会话上报已读：`GREATEST(last_read_seq, ?)` 保证只前进不回退，重复上报幂等。

### 4.4 朋友圈时间线

采用 **写扩散 + 拉模式降级** 的混合方案：

- 发布动态 → RabbitMQ 事件 → 消费者把 `moment_id` 写入好友的 Redis ZSet。
- 读取时先查 ZSet（命中足够条数就直接回表），否则回源 MySQL 拉模式。
- 拉黑关系**双向过滤**（我拉黑的、拉黑我的），否则会出现「我拉黑了他，他还能看到我」。

---

## 五、缓存策略

| 问题 | 手段 |
|---|---|
| 缓存击穿（热点 Key 失效瞬间打穿） | `singleflight` 合并同 Key 回源 |
| 缓存穿透（查不存在的数据） | 空值占位 `\x00` + 短 TTL |
| 缓存雪崩（大批 Key 同时失效） | 过期时间加 0~60s 随机抖动 |
| 关系判断热点 | 好友/拉黑/群成员结果缓存 5 分钟，变更时双向失效 |

`Remember` 的语义：先 GET，命中即返回；未命中走 singleflight 回源并回填；
回源返回 `ErrNotFound` 写入空值标记。缓存自身异常不阻断主流程，直接回源。

---

## 六、异步链路（RabbitMQ）

| 项 | 做法 |
|---|---|
| 拓扑 | `im.events`（direct）→ `im.events.q`，绑定各事件 routing key |
| 死信 | `im.dlx` → `im.events.dlq` |
| 生产 | channel 池（默认 8），publisher confirm，`DeliveryMode=persistent` |
| 自愈 | 连接断开由后台 `NotifyClose` 监听触发，指数退避重连（1s→30s），重连后重建池 |
| 消费 | 每个 worker 独占一条 channel（各自 `Qos`+`Consume`），手动 ACK，单条超时 15s |
| 并发 | `consumer_workers`（默认 3）；未确认消息总量 = `prefetch × workers` |
| 重试 | 重新发布并累加 `x-retry`，超过 3 次转死信 |

**为什么并发靠多 channel 而不是多 goroutine**：`amqp.Channel` 不是并发安全的，
一条 channel 上做并发消费只能串行化，等于没加并发。所以 worker 数 = channel 数。

**踩坑记录**：`Nack(requeue=true)` 不会修改消息 header，靠它做重试计数会导致
`x-retry` 永远是 0，形成无限重试。必须重新 publish 一条带新 header 的消息再 ACK 原消息。

**踩坑记录**：`x-retry` 的整数类型由**发送方**决定（本端发 `int32`，其他语言客户端常发
`int64`，JSON 中转会是 `float64`）。只断言一种类型会退化成 0，同样变成无限重试，
因此解析处按类型 switch 兜底。

**踩坑记录**：生产者早期没有重连，`Consumer` 有重连而 `Producer` 没有——broker 抖动一次，
之后所有 `Publish` 永久失败，而调用方大多只 Warn 或忽略错误，事件静默丢失。
现在是两侧都有退避重连，且 `Publish` 在重连窗口内快速失败，不阻塞业务主流程。

MQ 未启用（`rabbitmq.enabled=false`）时自动降级为本地内存总线：
同一个 `Registry` 注册处理器，单 worker 串行消费，缓冲满则丢弃并计数。
业务代码不感知差异。

---

## 六之二、原子性与事务边界

判断标准只有一条：**任何一步单独失败，是否会留下不可自愈的脏状态**。

### 必须用 Lua（Redis 侧）

Pipeline 只省往返，既不是事务也不保证全部执行，下列场景一律走脚本：

| 操作 | 拆开写的后果 | 脚本做的事 |
|---|---|---|
| 上报已读 | 裸 `HSET` 会让已读序号回退，群未读按 `last_seq − read_seq` 实时算，立刻反弹；序号前进而清未读失败，私聊角标一直挂着 | `HSET` 取 `max(旧值, 新值)`，同时 `HDEL` 未读、`PEXPIRE` 续期，返回最终序号 |
| 会话序号 | `INCR` 成功而 `PEXPIRE` 丢失，键回到永久驻留，一个会话一条、随会话数线性增长且没有任何回收机制 | `INCR` 后立即 `PEXPIRE`（7 天滑动）；过期由 `MAX(seq)` 回源重建，只多一次查询、不丢正确性 |
| 分布式限流 | ①`INCR` 成功而 `EXPIRE` 丢失，key 无 TTL，计数单调递增，该用户被**永久限流**；②先读计数再判断再写，并发下会超发；③固定窗口在窗口边界能漏出双倍额度 | zset 存放行时刻，一次脚本内完成「清理滑出窗口的记录 → 统计 → 判断 → 写入 → 续期」，被拒请求不写成员 |
| 设备在线 | 多端共用一个 key，任一设备心跳都会刷新整 key TTL，已断开设备被无限期保留，`IsOnline` 恒为 true | zset 的 score 存**设备自身**过期时刻，写入时顺带清理过期成员 |
| 未读计数自增 | `HINCRBY` 成功而 `PEXPIRE` 丢失，该用户的未读 hash 变永久键：一个用户一条、field 数等于他的会话数，随用户数单调增长 | 一条脚本内批量 `HINCRBY` 所有接收者，并逐个 `PEXPIRE`，计数与 TTL 同生 |
| 朋友圈时间线 | `EXPIRE` 丢失会让 zset 无 TTL 长期驻留 | `ZADD` + 裁剪 + `PEXPIRE` 一次执行 |

不需要 Lua 的：`SETNX`（消息幂等）本身就是原子命令，包一层脚本只剩开销。
`HSETNX`（入群对齐已读）用 pipeline 即可 —— 续期失败最多让键早一期过期，
缺失的已读位置下次 `MarkRead` 就会补回来，属于可自愈状态。

### 必须用事务（MySQL 侧）

| 操作 | 拆开写的后果 |
|---|---|
| 消息落库 + 会话/群指针 | 私聊：消息进库但列表不刷新，有红点点不进去；群聊：`last_msg_seq` 停住，读扩散下**全群**未读少算 |
| 入群 + 人数自增 | 自增失败则 `member_count` 永久偏小，上限判断失真，可越界入群 |
| 退群 + 人数自减 | 原实现用 `_ =` 吞错，人数永久偏大，之后入群被提前判定"已达上限" |
| 点赞/取消 + 计数 | 写成功但计数失败会返回 500，重试又被唯一索引判重复，计数**永远差 1** |
| 评论/删评论 + 计数 | 评论表无幂等键，插入成功但报错会让用户重试产生重复评论 |

### 事务之外还需要锁

「先查上限再插入」是典型的先读后写。事务本身不阻止两个并发事务读到同一份旧值，
因此 `AddMembers` / `RemoveMember` 在事务内额外 `SELECT ... FOR UPDATE` 锁群行，
用**锁内的** `member_count` 做判断。实测：995 人的群并发发起 3 次各 3 人的邀请，
只有 1 次成功，最终 `member_count = 998 = COUNT(*)`。

群消息指针则用条件更新代替锁：`UPDATE groups SET ... WHERE id = ? AND last_msg_seq < ?`。
并发发送时事务提交顺序可能与 seq 顺序相反，无条件覆盖会让指针倒退。

### 跨存储的边界

Redis 与 MySQL 无法同事务，只能选一个方向保证：

- **MySQL 为准**：先写库再写缓存，缓存失败只降级不报错，读取时按库重建。
- **可重算优先**：群未读不落存储（`last_seq − read_seq` 实时算），缓存丢了也不影响正确性。
- 私聊未读是唯一"可能少 1"的地方，兜底是会话行里的 `last_msg_id` —— 点进去就能看到消息本体。

---

## 七、性能数字参考

单机（4C8G，MySQL/Redis 同机）经验值，仅作量级参考：

| 场景 | 预期 |
|---|---|
| 单条私聊消息端到端 | 5~15ms（含落库 + 推送） |
| 单机 WS 长连接 | 3~5 万（受内存与 fd 限制） |
| 1000 人群单条消息 | 5 条 SQL、0 行会话写入，实测 ~26ms |
| HTTP 接口 P99（本地） | < 30ms |

压测方法：

```bash
go run ./cmd/smoke -base http://127.0.0.1:8080          # 功能回归（45 项）
curl http://127.0.0.1:8080/metrics                      # 在线连接、上下行帧数、goroutine、堆内存
go tool pprof http://127.0.0.1:8080/debug/pprof/profile?seconds=30
```

---

## 八、已知取舍

| 取舍 | 原因 | 后续方案 |
|---|---|---|
| 私聊写扩散 | 成员恒为 2，读取零计算 | 无需调整 |
| 群聊读扩散 | 写入与成员数解耦，O(N) → O(1) | 群数量级过万时把群列表缓存进 Redis |
| 私聊未读存 Redis | 高频读写 | Redis 不可用时读 `conversation_reads` 兜底 |
| 群未读不落存储 | 用 seq 差值算，天然无写放大 | 无需调整 |
| 单实例 Hub | 部署简单、延迟最低 | 已预留 Redis 总线，配置即切换 |
| 图片存本地磁盘 | 零依赖 | 抽象成 URL，换 OSS 只改 upload |
| 消息表未分片 | 单表千万级仍可支撑 | 按 `conv_id` 哈希或时间分区 |

---

## 九、可用性措施

- **优雅停机**：先 `server.Shutdown` 停止新请求 → 断开所有 WS → 停止消费 → 关闭连接。
- **健康检查** `/healthz` 探测 Redis 连通性，供负载均衡摘除。
- **超时全覆盖**：`http.Server` 的 ReadHeader/Read/Write/Idle 全设，请求级 context 超时，
  MQ 消费与 Redis 操作都有独立超时。
- **panic 兜底**：中间件捕获并转 500，进程不退出。
- **日志**：结构化 `slog`，release 模式输出 JSON；SQL 慢查询阈值 200ms。
