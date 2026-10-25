# Redis Key 手册

本文是 IMChat 项目 Redis 命名空间的完整说明：**每个 Key 的数据结构、生命周期、使用场景、承载功能，以及丢失后的自愈路径**。

> 代码基线：`internal/cache/*.go`（唯一登记处为 `cache.go` 的常量表）

---

## 0. 使用规范

| 规则 | 说明 |
|---|---|
| 命名格式 | `im:<域>:<标识>`，域是业务模块，标识是主键拼装 |
| 唯一登记处 | 全部前缀定义在 `internal/cache/cache.go` 的常量表；新增 Key 必须先在那里登记，再加访问方法 |
| 必须有 TTL | 除 Pub/Sub 频道外，不允许永久键。永久键会随用户数、会话数线性增长且无人回收 |
| 只存派生状态 | MySQL 是唯一真相。Redis 里任何数据丢失后，业务必须能靠回源或重算恢复，不能变成「丢了就错」 |
| 写多命令用 Lua | 两条以上写命令之间存在因果依赖时用 Lua；只是省往返可用 pipeline，但**不能**用它保证「要么都成功」 |
| 读穿统一入口 | 缓存读取走 `Cache.Remember`：`singleflight` 合并并发回源（防击穿）、`\x00` 空值占位 1 分钟（防穿透）、TTL 随机抖动（防雪崩） |

**Key 总数**：13 个 key 前缀 + 1 个 Pub/Sub 频道。

---

## 1. 全景表

| Key 模式 | 类型 | TTL | 模块 | 一致性角色 |
|---|---|---|---|---|
| `im:user:{uid}` | string | 10m + 抖动 | 用户 | 读穿缓存 |
| `im:rel:friend:{a}:{b}` | string | 5m + 抖动 | 关系 | 判定结果缓存 |
| `im:rel:blocked:{a}:{b}` | string | 5m + 抖动 | 关系 | 判定结果缓存 |
| `im:gmemberids:{gid}` | string | 5m + 抖动 | 群 | 读穿缓存 |
| `im:seq:{convID}` | string | 7d 滑动 | 消息 | **权威计数器**（唯一能无损重建） |
| `im:dedup:{uid}:{clientMsgID}` | string | 10m | 消息 | 幂等标记 |
| `im:unread:{uid}` | hash | 30d 滑动 | 消息 | 派生计数（可重算） |
| `im:read:{uid}` | hash | 30d 滑动 | 消息 | 派生进度（可回源） |
| `im:online:{uid}` | zset | 150s | 实时 | 心跳派生状态 |
| `im:feed:{uid}` | zset | 7d 滑动 | 朋友圈 | 写扩散索引（可重建） |
| `im:token:deny:{sha256}` | string | JWT 有效期 | 安全 | 拒绝名单 |
| `im:loginfail:{username}` | string | 5m 滑动 | 安全 | 计数 |
| `im:rate:{rule}:{subject}` | zset | 1m | 限流 | 窗口计数 |
| `im:bus` | Pub/Sub 频道 | — | 实时 | 跨节点广播 |

---

## 2. 用户与关系域

### 2.1 `im:user:{uid}` — 用户资料

| 项 | 内容 |
|---|---|
| 结构 | string，值为 `PublicUser` 的 JSON（**不含 `username`**，登录名等同 QQ 号不对外暴露） |
| TTL | `cache.user_ttl`（默认 10m）+ 随机抖动 ≤ `cache.ttl_jitter`（默认 60s） |
| 写入 | `Cache.LoadUser` → `Remember` 回源 `repository.UserRepo.GetByID` |
| 失效 | 修改资料后 `Cache.DelUser`（主动删，不等过期） |
| 特殊值 | `\x00` 空值占位，TTL 1 分钟 |

**使用场景**：`GET /users/:id` 查资料；会话列表、好友列表批量补昵称头像。

**功能**：把读多写少的资料查询挡在 MySQL 前面，同时用三道防护覆盖缓存三大经典问题——`singleflight` 防击穿、空值占位防穿透、TTL 抖动防雪崩。

**自愈**：删掉或过期后直接回源；用户不存在时回源一次后写 `\x00`，1 分钟内同一个不存在的 uid 不再打库。

> ⚠️ 实现要点：回源函数必须返回 `cache.ErrNotFound` 这个哨兵，`Remember` 才会写空值占位。返回 `resp.ErrNotFound`（业务错误）会导致占位从未生效、每次请求都打穿到 DB——这个坑已修。

---

### 2.2 `im:rel:friend:{a}:{b}` — 好友关系判定

| 项 | 内容 |
|---|---|
| 结构 | string，只有 `"1"`（是好友）和 `"0"`（不是）两种值 |
| TTL | `cache.friend_ttl`（默认 5m）+ 抖动 |
| 写入 | `Cache.FriendRelation` → 回源 `FriendRepo.IsFriend` |
| 失效 | 加好友 / 删好友 / 同意申请 → `Cache.DelRelations` |

**使用场景**：发消息前的可达性校验、朋友圈可见性判断、查询他人在线状态的权限校验。

**功能**：每条消息、每次打开朋友圈都会问一次，命中率极高，必须挡在 DB 之前。

**为什么双向各存一条**：查询天然是 `(a,b)` 和 `(b,a)` 两个方向交替来的，两个方向各留一条，避免一次查询把另一个方向挤掉。

---

### 2.3 `im:rel:blocked:{a}:{b}` — 拉黑关系判定

| 项 | 内容 |
|---|---|
| 结构 | string，`"1"`（存在拉黑关系）/ `"0"`（不存在） |
| TTL | `cache.friend_ttl`（默认 5m）+ 抖动 |
| 写入 | `Cache.BlockedRelation` → 回源 `FriendRepo.EitherBlocked` |
| 失效 | 拉黑 / 解除拉黑 / 删好友 → `Cache.DelRelations` |

**使用场景**：消息投递前的双向拦截、朋友圈互见判断。

**语义陷阱**：值代表的是「**任一方向**存在拉黑」的合并结论，不是「a 拉黑了 b」。所以失效时必须四条一起删（`a:b`、`b:a` 的 friend 与 blocked），只删一个方向会让另一个方向继续命中旧的 `"0"`，表现为「拉黑之后对方还能给我发消息」。

---

## 3. 群域

### 3.1 `im:gmemberids:{gid}` — 群成员 ID 列表

| 项 | 内容 |
|---|---|
| 结构 | string，值为 `[]int64` 的 JSON（只存 ID，成员角色/禁言状态另查） |
| TTL | `cache.group_member_ttl`（默认 5m）+ 抖动 |
| 写入 | `Cache.GroupMemberIDs` → 回源 `GroupRepo.MemberIDs` |
| 失效 | 建群 / 邀请 / 退群 / 踢人 / 解散 → `Cache.DelGroupMembers` |
| 特殊值 | `\x00` 空值占位（群成员为 0 时回源返回 `ErrNotFound`） |

**使用场景**：群消息扇出（决定推给谁）、群成员展示。

**功能**：群消息的接收者集合是发送链路上最重的一次查询，缓存后每条群消息只付一次 Redis 读。

**自愈**：过期或误删后回源 `group_members` 表；群已解散时返回空列表，缓存短期占位。

---

## 4. 消息域

消息域四个 Key 构成投递的计数基础：`seq` 负责排序，`unread`/`read` 必须成对维护，`dedup` 负责幂等。拆开写会出现「有红点看不到消息」这类状态错乱。

### 4.1 `im:seq:{convID}` — 会话消息序号

| 项 | 内容 |
|---|---|
| 结构 | string，十进制整数，`INCR` 语义 |
| TTL | 7 天滑动（每次分配续期），**比 unread/read 短** |
| 写入 | `Cache.NextSeq`（探测）/ `Cache.InitSeq`（初始化并分配），均为 Lua |
| 失效 | 只靠过期，没有主动删除 |

**使用场景**：每次发消息分配一个会话内严格递增的序号，写进 `messages.seq`，客户端靠它排重、断档检测、游标分页。

**为什么是唯一权威计数器**：`seq` 的连续性直接决定客户端能不能正确排序，所以它不能由 MySQL 自增 ID 代替（跨会话不连续），也不能在丢了之后从 1 重来。

**分配流程（两步，故意不合成一步）**：

```
NextSeq(convID)
  ├─ 键存在 → INCR + 续期，返回新序号
  └─ 键不存在 → 返回哨兵 0，不落值、不消耗号段
        └─ 调用方查 MAX(seq)
             └─ InitSeq(convID, floor)  ← 原子：cur < floor 才 SET，然后 INCR
```

- 键不存在时返回 0 而不是直接 `INCR`：裸 `INCR` 会从 1 开始，与库里历史 seq 重叠，客户端会把新消息当旧消息丢弃。
- `initSeqScript` 用 `cur < floor` 先比后写：并发请求都探测到空键时，第一个设好 floor，其余看到的 cur 已更大，只做 INCR，天然幂等。
- 「探测」与「分配」分开的另一层用意：探测不消耗号段，并发请求会一起走到初始化路径，由脚本收敛到同一段序号。

**自愈**：这是全项目**唯一能无损重建**的计数状态。7 天没有新消息的冷会话会自然过期，下次发消息时回源 `MAX(seq)`（走 `uk_conv_seq` 索引）重建，正确性不变，代价只是一次索引查询。

---

### 4.2 `im:dedup:{fromID}:{clientMsgID}` — 客户端消息幂等

| 项 | 内容 |
|---|---|
| 结构 | string `"1"`，`SETNX` 写入 |
| TTL | 10 分钟（代码内常量，未进配置） |
| 写入 | `Cache.MarkClientMsg`，首次返回 `true`，重复返回 `false` |

**使用场景**：弱网重发、客户端点击重试、断线重连后补发。

**功能**：同一发送者在 10 分钟内用同一个 `client_msg_id` 只会落一条消息。

**兜底**：MySQL 侧有 `uk_from_client` 唯一索引作为最终防线。Redis 这份是为了避免大部分重复请求走到数据库才被拒绝；即使 Redis 丢键，DB 也会用 1062 回查到已落库消息并按成功返回。

---

### 4.3 `im:unread:{uid}` — 会话未读数

| 项 | 内容 |
|---|---|
| 结构 | **hash**，field = `conv_id`，value = 未读条数 |
| TTL | 30 天，每次自增续期 |
| 写入 | `Cache.IncrUnread`（Lua：批量 `HINCRBY` + `PEXPIRE`） |
| 失效 | 上报已读时 `markReadScript` 内 `HDEL` 掉该 field |

**使用场景**：会话列表的私聊红点。

**为什么群聊不用它**：群未读 = 群最新 seq − 我的已读 seq，实时算即可，不需要为每个群成员维护一个计数（否则一个 500 人群的每条消息要写 500 次计数）。

**自愈**：过期后私聊未读退化为按会话行里的 `last_msg_id` 兜底，点进去就能看到全部消息，不会丢消息。

**为什么自增与续期必须同一条 Lua**：`HINCRBY` 成功而 `PEXPIRE` 丢失，这个用户的未读 hash 就成了永久键——一个用户一条、field 数等于他的会话数，随用户数单调增长且没有任何回收路径。pipeline 只省往返，不保证全部执行。

---

### 4.4 `im:read:{uid}` — 会话已读位置

| 项 | 内容 |
|---|---|
| 结构 | **hash**，field = `conv_id`，value = 已读到的 `seq` |
| TTL | 30 天，每次上报续期 |
| 写入 | `Cache.MarkRead`（Lua）、`Cache.SetReadSeqIfAbsent`（pipeline + `HSETNX`） |
| 失效 | 只靠过期 |

**使用场景**：群未读计算、多端已读同步、已读回执。

**`markReadScript` 一次完成三件事**（必须一起成立）：

1. 已读序号**只前进不后退**（乱序上报时回退会让群未读突然变大，DB 侧有 `GREATEST`、Redis 侧没有）；
2. `HDEL` 掉未读计数（只前进不清计数 → 私聊角标一直挂着）；
3. `PEXPIRE` 续期（拆开写一旦续期丢失，hash 变永久键）。

对「重复上报、位置未前进」的调用同样续期——只要用户还在上报，说明会话仍活跃。

**`SetReadSeqIfAbsent` 为什么可以用 pipeline**：`HSETNX` 与 `PEXPIRE` 之间没有因果依赖，续期失败最多让键早一期过期，而缺失的已读位置下次上报就会补回来，不属于「不可自愈」的脏状态。

**自愈**：`conversation_reads` 表是回源兜底。读取会话列表时若发现某会话的已读位置缺失，会批量回源该表；从未上报过已读的会话则按群最新 seq 对齐，不会凭空冒出几千条未读。

---

## 5. 实时域

### 5.1 `im:online:{uid}` — 设备在线状态

| 项 | 内容 |
|---|---|
| 结构 | **zset**，member = 设备标识（`web`/`android`/…），score = **该设备自己的过期时刻**（毫秒时间戳） |
| TTL | key 级 150s = `ws_ping_interval`(30s) × (2 × 2 + 1)；成员级由 score 表达 |
| 写入 | 握手 `Hub.MarkOnline`；心跳每 2 次 `Client.renewOnline` 一次；退出 `Hub.markOffline` → `ZREM` |
| 判定 | `ZCOUNT key <now> +inf > 0`，**不看 key 级 TTL** |

**使用场景**：判断接收者是否在线，决定要不要投离线消息；查询用户在线状态（仅限自己或好友）。

**为什么 score 存过期时刻而不是用 set + key TTL**：多端共用一个 key，任一设备心跳都会刷新整个 key 的 TTL，已断开设备的成员就会被无限期保留，导致 `IsOnline` 恒为 true。把过期时刻放进 score，`ZREMRANGEBYSCORE` 每次写入顺手清理，单设备掉线不会被其他设备「续命」。

**集群读取策略**：先查本地分片 map，未命中且开启集群时才回源 Redis。单机部署就是本地这条路径，零 Redis 往返，也不存在 TTL 窗口造成的假离线。

> ⚠️ `cluster.enabled = false` 时**完全不会写这个 key**，单机部署在 Redis 里看不到在线数据是预期行为，不是写入失败。

---

### 5.2 `im:bus` — 跨节点推送总线

| 项 | 内容 |
|---|---|
| 结构 | Pub/Sub 频道（不是 key，无 TTL、不持久化） |
| 消息体 | `{"origin": 节点ID, "uids": [...], "payload": 原始帧字节}` |
| 写入 | `Hub.publishBusSync`，2 个 worker 协程异步发布 |
| 读取 | `Hub.subscribeBus` 订阅，跳过 `origin == 自己` 的消息避免重复推送 |

**使用场景**：多实例部署时，用户连在 A 节点、消息从 B 节点发出，靠它把帧送到 A。

**局限**：Pub/Sub 不落盘，节点掉线期间的消息直接丢失，所以离线补偿走 RabbitMQ 的离线队列，不依赖总线可靠性。

---

## 6. 朋友圈域

### 6.1 `im:feed:{uid}` — 好友动态时间线

| 项 | 内容 |
|---|---|
| 结构 | **zset**，member = `moment_id`，score = 发布时刻（毫秒） |
| TTL | 7 天滑动（每次写入续期） |
| 写入 | `Cache.PushFeed`（Lua：`ZADD` + `ZREMRANGEBYRANK` 裁剪 + `PEXPIRE`） |
| 裁剪 | 只保留最近 `maxFeedFanout × 2` = 1000 条 |

**使用场景**：好友发布动态后写扩散到每个好友的时间线；读取时先查这里。

**读路径**：`ZREVRANGEBYSCORE` 取一页 ID → 命中数量够就只按 ID 批量查详情；不够则回源 MySQL（拉模式降级）。

**为什么要裁剪**：时间线是典型的「只关心最近」的数据，不裁剪的话活跃用户的时间线会无限增长，而 Redis 内存是有限资源。

**自愈**：时间线本身完全可由 `moments` 表按发布时间重建，丢了只影响一次读取性能，不影响正确性。

**写扩散的边界**：好友数超过 `maxFeedFanout`（500）时直接放弃写扩散，只用拉模式，避免一条动态写 500 次 Redis。

---

## 7. 安全与限流域

### 7.1 `im:token:deny:{sha256}` — 登出令牌黑名单

| 项 | 内容 |
|---|---|
| 结构 | string `"1"` |
| key 后缀 | **token 的 SHA-256 摘要**（64 位小写十六进制），不是明文 token |
| TTL | JWT 有效期（默认 168h，等于 `jwt.expire`） |
| 写入 | 登出 `Cache.DenyToken` |
| 读取 | 鉴权中间件 `Cache.IsTokenDenied`（HTTP 与 WS 握手都查） |

**使用场景**：登出后让无状态的 JWT 立即失效。

**为什么存摘要**：JWT 有 200+ 字符，明文拼进 key 会让每条记录的内存开销翻几倍，而且会原样出现在 `SCAN`、`MONITOR`、慢日志和监控面板里——等于把一份还能用的凭证抄到最容易泄露的地方。摘要不损失唯一性：校验时用同一个 token 算摘要，能命中就说明已登出。

**已知限制**：TTL 传的是令牌**完整有效期**而不是剩余有效期，接近过期的令牌登出后 key 会多留一段时间。属于「多占内存但不会漏判」的安全方向，暂未修改。

---

### 7.2 `im:loginfail:{username}` — 登录失败计数

| 项 | 内容 |
|---|---|
| 结构 | string，十进制计数 |
| TTL | 5 分钟，**每次失败续期**（从最后一次失败算起） |
| 写入 | `Cache.RecordLoginFailure`（Lua：`INCR` + `PEXPIRE`），登录成功 `ResetLoginFailures` 删除 |

**使用场景**：连续失败 5 次锁定 5 分钟，抵御撞库。

**功能**：用户名不存在也照样计数，避免被用来枚举用户。

**为什么必须用 Lua**：一旦 `INCR` 成功而 `PEXPIRE` 丢失，计数会停在阈值上且永不过期，而读路径发现超阈值就直接拒绝——后续不会再有失败请求来补 TTL，这个账号就被**永久锁死**了。

**注意**：这里只做账号维度，IP 维度在 `im:rate:auth:ip{...}` 兜底（换用户名绕不过 IP 限流）。

---

### 7.3 `im:rate:{rule}:{subject}` — 滑动窗口限流

| 项 | 内容 |
|---|---|
| 结构 | **zset**，member = `{毫秒时间戳}:{进程内自增序号}`，score = 毫秒时间戳 |
| TTL | 等于窗口长度（当前 `write` / `auth` 都是 1 分钟） |
| 写入 | `Cache.AllowSlide`（Lua：`ZREMRANGEBYSCORE` 清窗口外 + `ZCARD` 判额度 + `ZADD`） |

**Key 的实际取值**（只有带 `Global` 的规则才会产生这个 key）：

| rule | subject | 完整 key 示例 | 配额 |
|---|---|---|---|
| `write` | `u{uid}` | `im:rate:write:u10086` | 120 次 / 分钟（跨实例共享） |
| `auth` | `ip{ip}` | `im:rate:auth:ip1.2.3.4` | 30 次 / 分钟（跨实例共享） |

`read` / `upload` / `ws` 三条规则只有进程内令牌桶，**不产生 Redis key**。

**使用场景**：发消息等写接口的总量封顶；登录、注册按 IP 限制撞库尝试次数。

**为什么用滑动窗口而不是固定窗口**：固定窗口在边界会漏出双倍额度——第 59 秒打满一个窗口、第 61 秒再打满下一个，两秒内实际通过 `2 × limit`，对登录撞库等于限流形同虚设。

**为什么 member 要带自增序号**：同一毫秒内的并发请求 score 相同，member 也相同的话 `ZADD` 会覆盖而不是累加，窗口计数就会少算。

**拒绝路径不写成员**：被拒的请求不该继续占内存，否则攻击者能把一个 key 撑到无限大。`ZREMRANGEBYSCORE` 顺手清掉滑出窗口的旧记录，键大小稳定在 `limit` 量级。

**故障策略**：Redis 异常时放行，由本地令牌桶兜底——限流组件故障不该把登录和发消息一起掐死。

---

## 8. TTL 与容量估算

### 8.1 TTL 一览

| Key | TTL | 来源 | 续期时机 |
|---|---|---|---|
| `im:user:` | 10m + 抖动 | `config.cache.user_ttl` | 回源写入时 |
| `im:rel:friend:` / `im:rel:blocked:` / `im:gmemberids:` | 5m + 抖动 | `config.cache.friend_ttl` / `group_member_ttl` | 回源写入时 |
| `im:seq:` | 7d | 代码常量 `seqTTL` | 每次分配 |
| `im:dedup:` | 10m | 代码常量 | 不续期 |
| `im:unread:` / `im:read:` | 30d | 代码常量 | 每次写入/上报 |
| `im:online:` | 150s | `ws_ping_interval × 5` | 每次心跳（约 60s） |
| `im:feed:` | 7d | 代码常量 | 每次写扩散 |
| `im:token:deny:` | 168h | `jwt.expire` | 不续期 |
| `im:loginfail:` | 5m | `loginFailWindow` | 每次失败 |
| `im:rate:` | 1m | `config.ratelimit.*_window` | 每次判定 |

### 8.2 容量粗算

单条 key 的 Redis 自身开销约 50–80 字节（dict entry + redisObject），zset 每个成员再加 60–90 字节。

| Key | 条数上限 | 量级驱动 |
|---|---|---|
| `im:user:` | 10 分钟内活跃用户数 | DAU |
| `im:rel:*` | 4 × 被查询过的好友对 | 好友关系数 × 查询覆盖率 |
| `im:gmemberids:` | 群数 | 群规模 |
| `im:seq:` | 7 天内活跃会话数 | 活跃度 |
| `im:unread:` / `im:read:` | 用户数 × 平均会话数（field 数） | 用户规模 × 会话密度 |
| `im:online:` | 在线用户 × 设备数 | 同时在线数 |
| `im:feed:` | 用户数 × min(好友动态数, 1000) | 用户规模 × 动态量 |
| `im:rate:` | 活跃主体数 × ≤ limit | QPS |

> 经验值：`im:unread:` / `im:read:` 是内存增长最快的一组（一个用户 N 个会话就是 N 个 field），`im:online:` 是写最频繁的一组（每个在线连接约 60 秒一次 `ZADD`）。

---

## 9. 设计取舍与已知限制

1. **`read` / `upload` / `ws` 没有跨实例总量封顶**。这三条高频规则只走进程内令牌桶，集群下实际配额 = 单机配额 × 节点数。这是刻意的取舍：给高频读接口每次加一次 Redis 往返，代价大于收益。需要全局封顶时，把它们改成 `write` / `auth` 那种带 `Global` 的规则，或在入口层（网关/Ingress）限流。
2. **`ws_max_conns` 是单用户单节点上限**。多实例部署下单用户可连 `max_conns × 节点数` 个连接。精确全局计数需要每次握手一次 Redis 往返，当前判断连接数已被握手限流压住，取本地判断。
3. **单机模式不写 `im:online:`**。`cluster.enabled = false` 时在线态完全靠本地分片 map，Redis 里查不到是预期行为。
4. **`im:seq:` / `im:read:` / `im:unread:` 过期是常态而非异常**。三者都有明确的自愈路径（回源 `MAX(seq)`、回源 `conversation_reads`、按 `last_msg_id` 兜底），所以 TTL 可以设得比「永久」短得多，避免冷数据长期占内存。
5. **`im:bus` 不持久化**。节点掉线期间的总线消息会丢，离线补偿由 RabbitMQ 离线队列负责，两者职责不同不能互相替代。
6. **部分 TTL 仍是代码常量**：`im:dedup:`（10m）、`im:feed:`（7d）、`im:unread:`/`im:read:`（30d）、`im:seq:`（7d）。当前不需要按环境调整，若要支持灰度调参需迁到 `config.cache`。
7. **多 Key 的 Lua 脚本在 Redis Cluster 下会 `CROSSSLOT`**。`incrUnreadScript` 一次要写多个用户的 `im:unread:`，不同 uid 天然不同槽。当前用的是单机客户端，没有这个问题；将来切 Cluster 客户端必须按 uid 分批执行或退回 pipeline（后者会失去「计数与续期同生」的保证）。
8. **群成员列表为空的处理**：`im:gmemberids:` 回源拿到空列表时视为 `ErrNotFound` 并写空值占位。正常路径不可达（发消息前已校验发送者是群成员），仅在「最后一人退群的同时有人发消息」这种极端时序下会以 500 收场，属已知边界。

---

## 10. 变更记录

记录的是「为什么这样设计」而不是「什么时候改的」，后者交给 git 历史。

| 变更 | 原因 |
|---|---|
| `im:rel:friend:` / `im:rel:blocked:` / `im:gmemberids:` 收归 `cache` 包 | 这三个前缀原先硬编码在 `service/relation.go`，绕过了「Key 唯一登记处」的约定，且 TTL 写死 5 分钟，`config.cache.friend_ttl` / `group_member_ttl` 成了死配置 |
| 删除 `im:friend:` / `im:blocked:` / `im:gmember:` | 三个前缀只有删除调用没有读取方，每次好友变更、拉黑都白跑一次 `DEL`；群成员缓存同时存在 `im:gmember:` 与 `im:gmemberids:` 两套，前者从未被读写 |
| `im:token:deny:` 后缀从明文 JWT 改为 SHA-256 摘要 | 避免凭证出现在 `SCAN` / `MONITOR` / 慢日志中，同时把 key 从 200+ 字符压到 64 字符 |
| `im:unread:` 自增从 pipeline 改为 Lua | 保证「计数与 TTL 同生」，与 `im:seq:` / `im:read:` 的处理方式统一 |
| 修复 `im:user:` 空值占位从未生效 | `Profile` 回源返回 `resp.ErrNotFound`，`Remember` 只认 `cache.ErrNotFound`，导致查不存在的 uid 每次都打穿 DB |

---

## 11. 运维速查

```bash
# 按前缀盘点（生产环境禁止直接用 KEYS，务必用 SCAN）
redis-cli --scan --pattern 'im:user:*' | head -20

# 看某个 key 的类型、TTL、元素数
redis-cli TYPE   im:online:10086
redis-cli TTL    im:online:10086
redis-cli ZCARD  im:online:10086

# 某个用户的全部未读 / 已读
redis-cli HGETALL im:unread:10086
redis-cli HGETALL im:read:10086

# 某会话的序号
redis-cli GET im:seq:group:42

# 观察在线设备与其过期时刻（score 是毫秒时间戳，小于 now 即已过期）
redis-cli ZRANGE im:online:10086 0 -1 WITHSCORES

# 顶掉某个用户的登出黑名单（升级前的明文 token key 需要清掉）
redis-cli --scan --pattern 'im:token:deny:*' | xargs -r -n 100 redis-cli DEL

# 实时看跨节点广播
redis-cli SUBSCRIBE im:bus
```

**排障顺序建议**：会话列表红点异常 → 先看 `im:read:` / `im:unread:`；消息序号冲突 → 看 `im:seq:` 是否存在及值；推送丢消息 → 先确认 `im:bus` 订阅是否活着，再看 RabbitMQ 离线队列。
