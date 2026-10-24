# 数据模型参考

本文是 `internal/model/` 的字段级说明，用于排查线上数据、写迁移脚本、评估加字段的影响面。
设计取舍的来龙去脉见 [architecture.md](./architecture.md) 第四章，这里只讲「有什么、干什么用」。

表结构由 `cmd/server` 启动时的 `AutoMigrate(model.AllModels())` 创建，`deploy/init/01-init.sql`
只负责建库与字符集。因此**模型上的 tag 就是表结构的唯一事实来源**，改结构即改 tag。

---

## 一、总览

12 个实体，5 个业务域，全部在 `internal/model` 包：

| 表 | 实体 | 职责 | 主要访问路径 |
|---|---|---|---|
| `users` | `User` | 账号与资料 | 登录、资料页、关系补全 |
| `friends` | `Friend` | 好友关系（双向各一行） | 好友列表、关系判定（带缓存） |
| `friend_requests` | `FriendRequest` | 好友申请流水 | 申请列表、审批 |
| `blacklists` | `Blacklist` | 拉黑 | 消息拦截、朋友圈可见性 |
| `groups` | `Group` | 群资料 + 最新消息指针 | 群列表、群详情、会话列表组装 |
| `group_members` | `GroupMember` | 群成员与角色 | 权限校验、消息扇出 |
| `messages` | `Message` | 私聊 + 群聊消息 | 历史分页、撤回 |
| `conversations` | `Conversation` | 私聊会话列表项 | 会话列表、置顶/免打扰 |
| `conversation_reads` | `ConversationRead` | 已读位点 | 未读计算、多端同步 |
| `moments` | `Moment` | 朋友圈动态 | 时间线、动态详情 |
| `moment_likes` | `MomentLike` | 点赞 | 点赞列表、幂等 |
| `moment_comments` | `MomentComment` | 评论与回复 | 评论列表 |

`PublicUser` 不是表，是 `User.ToPublic()` 生成的对外投影，只保留
`id / username / nickname / avatar / gender / signature`，把 `password_hash`、`phone`、`status` 挡在响应之外。

---

## 二、通用约定

### 2.1 主键与时间戳

除 `Message` 外所有实体嵌入 `BaseModel`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | `int64` | 雪花 ID，`primaryKey;autoIncrement:false`，由 `BaseModel.BeforeCreate` 兜底分配 |
| `created_at` | `time.Time` | 创建时间，单列索引 |
| `updated_at` | `time.Time` | GORM 自动维护（`Update` 会刷新，`UpdateColumns` 不会） |

`Message` 也嵌入 `BaseModel`，因此同样拥有这三个字段。**所有写入路径都不要手动生成 ID**，
交给 `BeforeCreate`，避免出现「某个新入口忘了赋值 → 主键写成 0 → 并发插入冲突」。

### 2.2 JSON 序列化

- 所有 `int64` 主键/外键都带 `json:"...,string"`。前端 JS 的 `Number` 只有 53 位精度，雪花 ID 直接输出会丢位。
- `StringList` 是 `[]string` 的包装，实现 `driver.Valuer` / `sql.Scanner`，以 JSON 文本落 `text` 列，目前只用于 `moments.images`。

### 2.3 删除策略

| 策略 | 使用位置 | 原因 |
|---|---|---|
| 状态位置删 | `messages.status`、`moments.status` | 消息要展示「已撤回」，动态要支持恢复与审计 |
| 物理删除 | `friends`、`moment_likes`、`moment_comments` | 删除即无意义数据，靠唯一索引保证不重复 |
| 只增不改 | `friend_requests` | 申请历史用于审计与风控 |

### 2.4 外键与一致性

**数据库层不建外键**（GORM 默认行为），跨表一致性全部由 service 层事务保证。典型组合：

- 发消息：`messages` 插入 + `conversations` 刷新（私聊）/ `groups` 更新指针（群聊）必须同事务，见 `service/message.go` 的 `persistAndDeliver`。
- 点赞/评论：明细行与 `moments` 冗余计数必须同事务，否则「写成功、计数失败」后重试会被唯一索引判为重复，计数永久偏小。

### 2.5 索引命名

- `uk_xxx`：唯一索引。
- `idx_xxx`：普通索引。
- 未显式命名的单列索引由 GORM 自动生成 `idx_<表名>_<字段名>`。

---

## 三、实体字段

### 3.1 users

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `username` | varchar(32) | 登录账号，**唯一索引**，注册后不可改 |
| `password_hash` | varchar(100) | 密码摘要，`json:"-"`，任何响应都不输出 |
| `nickname` | varchar(32) | 展示名，可重复、可改 |
| `avatar` | varchar(255) | 头像 URL |
| `gender` | tinyint | 0 未知 / 1 男 / 2 女，默认 0 |
| `signature` | varchar(255) | 个性签名 |
| `phone` | varchar(20) | 预留字段，**当前 service 层无任何读写**；非唯一索引 |
| `status` | tinyint | `UserStatusNormal(0)` / `UserStatusDisabled(1)`，索引供后台过滤 |
| `last_active_at` | datetime NULL | 最后活跃时间，指针用于区分「从未活跃」与「零值」 |
| `created_at` / `updated_at` | datetime | BaseModel |

索引：`PRIMARY(id)`、`uniq(username)`、`idx(phone)`、`idx(status)`、`idx(created_at)`。

### 3.2 friends

好友关系**双向各存一行**（A→B 与 B→A），列表页因此可以「按 user_id 前缀扫描 + 索引排序」，不必 `OR` 两列。

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `user_id` | int64 | 关系持有方 uid |
| `friend_id` | int64 | 对方 uid |
| `remark` | varchar(32) | 好友备注名，只影响持有方展示 |
| `starred` | bool | 星标好友，默认 false |

索引：`PRIMARY(id)`、`uk_user_friend(user_id, friend_id) UNIQUE`。
注意：删除好友是物理删除，不留历史；重复添加靠唯一索引拦。

### 3.3 friend_requests

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `from_user_id` | int64 | 申请人 |
| `to_user_id` | int64 | 接收人 |
| `message` | varchar(255) | 验证消息 |
| `status` | tinyint | 0 待处理 / 1 已通过 / 2 已拒绝，**保留历史** |

索引：`PRIMARY(id)`、`idx_from_to(from_user_id, to_user_id)`、`idx_to_status(to_user_id, status)`。
`idx_to_status` 直接服务「我的待处理申请」；`idx_from_to` 服务「我发给某人的申请是否还挂着」。

### 3.4 blacklists

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `user_id` | int64 | 拉黑发起方 |
| `blocked_user_id` | int64 | 被拉黑方 |

索引：`PRIMARY(id)`、`uk_user_blocked(user_id, blocked_user_id) UNIQUE`、`idx(blocked_user_id)`。
判定用 `EitherBlocked`（任一方向生效）：拉黑后消息发送被拒，双方朋友圈互不可见。

### 3.5 groups

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `name` | varchar(64) | 群名 |
| `avatar` | varchar(255) | 群头像 |
| `owner_id` | int64 | 群主 uid，索引 |
| `notice` | varchar(500) | 群公告 |
| `member_count` | int | 冗余成员数，列表页免 `count(*)` |
| `max_member` | int | 成员上限，默认 `MaxGroupMembers(1000)` |
| `status` | tinyint | 0 正常 / 1 已解散，索引 |
| `last_msg_id` | int64 | 最后一条消息 ID |
| `last_msg_seq` | int64 | 最后一条消息序号，**读扩散下未读计算的分母** |
| `last_msg_preview` | varchar(128) | 列表摘要，文本截断 40 字，图片/文件用占位文案 |
| `last_msg_type` | tinyint | 最后一条消息类型，前端决定渲染方式 |
| `last_msg_at` | datetime NULL | 最后消息时间；为 NULL 时列表展示 `created_at` |

索引：`PRIMARY(id)`、`idx(owner_id)`、`idx(status)`、`idx(created_at)`。

`UpdateLastMessage` 带 `WHERE last_msg_seq < ?` 条件，乱序到达的旧消息不会把指针回退。

### 3.6 group_members

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `group_id` | int64 | 群 ID |
| `user_id` | int64 | 成员 uid |
| `role` | tinyint | 0 成员 / 1 管理员 / 2 群主，`IsAdmin()` 判权限 |
| `group_nickname` | varchar(32) | 群内昵称，空则回落到 `users.nickname` |
| `muted` | bool | **群内禁言**（仅对普通成员生效，管理员与群主不受限） |
| `joined_at` | datetime | 入群时间，无默认值，**创建时必须显式赋值** |

索引：`PRIMARY(id)`、`uk_group_user(group_id, user_id) UNIQUE`、`idx_user_joined(user_id, joined_at)`。

> `group_members.muted` 是「禁言」，`conversations.muted` 是「免打扰」，同名字不同义，改代码时别串。

### 3.7 messages

私聊与群聊共表，靠 `conv_id` 区分；`seq` 是会话内的连续序号。

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键，雪花 ID |
| `conv_id` | varchar(64) | 会话标识，见 5.1 |
| `conv_type` | tinyint | 1 私聊 / 2 群聊 |
| `seq` | int64 | 会话内序号，Redis 分配，客户端判断断档 |
| `from_id` | int64 | 发送者，索引（拉黑校验、风控） |
| `to_id` | int64 | 私聊接收方；**群聊固定 0** |
| `group_id` | int64 | 群 ID；**私聊固定 0**，索引 |
| `client_msg_id` | varchar(64) | 客户端幂等键，**唯一索引**，全局唯一不区分发送者 |
| `msg_type` | tinyint | 1 文本 / 2 图片 / 3 文件 / 4 系统 |
| `content` | text | 正文；文本上限 `maxContentRunes(5000)`，由 service 校验 |
| `extra` | varchar(1024) | JSON 扩展：图片宽高、文件名等；**无长度校验，写入方自行控制在 1024 字节内** |
| `status` | tinyint | 0 正常 / 1 已撤回 |
| `created_at` | datetime | 发送时间，索引；撤回窗口按它判断 |
| `updated_at` | datetime | 状态变更时间（撤回会刷新） |

索引：`PRIMARY(id)`、`idx_conv_seq(conv_id, seq)`、`idx(from_id)`、`idx(group_id)`、`uk_from_client(client_msg_id) UNIQUE`、`idx(created_at)`。

`idx_conv_seq` 支撑 `WHERE conv_id = ? AND seq < ? ORDER BY seq DESC LIMIT n` 的游标分页，代价与页深无关。

### 3.8 conversations

私聊的会话列表项，每个参与者一行（写扩散）；群的会话行只在用户设置置顶/免打扰时才落库。

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `user_id` | int64 | 归属用户 |
| `conv_id` | varchar(64) | 会话标识 |
| `conv_type` | tinyint | 1 私聊 / 2 群聊 |
| `target_id` | int64 | 私聊为对方 uid，群聊为 gid；用于补资料与跳转 |
| `last_msg_id` | int64 | 最后一条消息 ID |
| `last_msg_preview` | varchar(128) | 摘要文案 |
| `last_msg_type` | tinyint | 最后一条消息类型 |
| `last_msg_at` | datetime | 列表排序键，索引 |
| `pinned` | bool | 置顶，排序时优先于时间 |
| `muted` | bool | **免打扰**，只影响推送提示 |

索引：`PRIMARY(id)`、`uk_user_conv(user_id, conv_id) UNIQUE`、`idx_user_last(user_id, last_msg_at)`、`idx(created_at)`。

### 3.9 conversation_reads

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `user_id` | int64 | 用户 |
| `conv_id` | varchar(64) | 会话 |
| `last_read_seq` | int64 | 已读到的 seq，**只前进不后退**（`GREATEST` 保证幂等） |

索引：`PRIMARY(id)`、`uk_user_conv(user_id, conv_id) UNIQUE`（与 `conversations` 同名但在不同表，不冲突）。

Redis 的 `im:read:{uid}` hash 是热数据，DB 是回源与冷启动的兜底。

### 3.10 moments

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `user_id` | int64 | 作者，索引 |
| `content` | text | 文字正文，上限 2000 字（service 校验） |
| `images` | text | `StringList` 序列化的 JSON 数组，上限 9 张 |
| `visibility` | tinyint | 1 公开 / 2 仅好友 / 3 私密；非法值在 service 层降级为 2 |
| `location` | varchar(64) | 位置文本 |
| `like_count` | int | 冗余点赞数，与明细同事务增减，`GREATEST(...,0)` 防负 |
| `comment_count` | int | 冗余评论数，同上 |
| `status` | tinyint | `MomentStatusNormal(0)` / `MomentStatusDeleted(1)`，索引 |
| `created_at` / `updated_at` | datetime | BaseModel |

索引：`PRIMARY(id)`、`idx_user_created(user_id)`、`idx(status)`、`idx(created_at)`。

> `idx_user_created` 名字里有 created，实际只有 `user_id` 一列（`created_at` 来自 `BaseModel`，无法挂到具名复合索引上）。查询是 `WHERE user_id = ? ORDER BY id DESC`，InnoDB 二级索引自带主键后缀，等价于 `(user_id, id)`，够用；后续若真按 `created_at` 排序，需在实体上显式重写该字段。

### 3.11 moment_likes

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `moment_id` | int64 | 动态 ID |
| `user_id` | int64 | 点赞者 |

索引：`PRIMARY(id)`、`uk_moment_user(moment_id, user_id) UNIQUE`。
点赞用 `ON CONFLICT DO NOTHING`，重复点赞 `RowsAffected=0`，据此决定是否加计数。

### 3.12 moment_comments

| 字段 | 类型 | 用途 / 约束 |
|---|---|---|
| `id` | int64 | 主键 |
| `moment_id` | int64 | 动态 ID |
| `user_id` | int64 | 评论者 |
| `content` | varchar(500) | 评论内容，1-500 字 |
| `parent_id` | int64 | 一级评论 ID；0 表示本身是一级评论 |
| `reply_to_user_id` | int64 | 被回复者，用于「回复 @某人」展示；0 表示无 |

索引：`PRIMARY(id)`、`idx_moment_created(moment_id)`。

> 与 `idx_user_created` 同理：名字带 created，实际只有 `moment_id` 一列。评论按 `id ASC` 排序，主键后缀已覆盖。

---

## 四、索引总览

| 表 | 索引 | 列 | 类型 | 服务的查询 |
|---|---|---|---|---|
| users | PRIMARY | id | 主键 | 按 ID 取用户 |
| users | idx_users_username | username | 唯一 | 登录、注册查重 |
| users | idx_users_phone | phone | 普通 | 预留 |
| users | idx_users_status | status | 普通 | 后台筛账号 |
| friends | uk_user_friend | user_id, friend_id | 唯一 | 好友列表、防重复 |
| friend_requests | idx_from_to | from_user_id, to_user_id | 普通 | 我发出的申请 |
| friend_requests | idx_to_status | to_user_id, status | 普通 | 待处理申请 |
| blacklists | uk_user_blocked | user_id, blocked_user_id | 唯一 | 防重复拉黑 |
| blacklists | idx_blacklists_blocked_user_id | blocked_user_id | 普通 | 反查拉黑关系 |
| groups | idx_groups_owner_id | owner_id | 普通 | 我创建的群 |
| groups | idx_groups_status | status | 普通 | 过滤已解散 |
| group_members | uk_group_user | group_id, user_id | 唯一 | 成员去重、权限校验 |
| group_members | idx_user_joined | user_id, joined_at | 普通 | 我加入的群 |
| messages | idx_conv_seq | conv_id, seq | 普通 | 历史游标分页 |
| messages | uk_from_client | client_msg_id | 唯一 | 消息幂等兜底 |
| messages | idx_messages_from_id | from_id | 普通 | 发送者维度查询 |
| messages | idx_messages_group_id | group_id | 普通 | 群消息维度查询 |
| messages | idx_messages_created_at | created_at | 普通 | 审计、归档 |
| conversations | uk_user_conv | user_id, conv_id | 唯一 | 定位单个会话 |
| conversations | idx_user_last | user_id, last_msg_at | 普通 | 会话列表排序 |
| conversation_reads | uk_user_conv | user_id, conv_id | 唯一 | 定位已读位点 |
| moments | idx_user_created | user_id | 普通 | 个人时间线 |
| moments | idx_moments_status | status | 普通 | 过滤已删除 |
| moment_likes | uk_moment_user | moment_id, user_id | 唯一 | 点赞幂等 |
| moment_comments | idx_moment_created | moment_id | 普通 | 取动态下评论 |

每张表还有 `PRIMARY(id)` 与 `idx_created_at`（`BaseModel.CreatedAt` 的 `gorm:"index"` 自动生成）。

---

## 五、关键机制

### 5.1 conv_id 规则

| 场景 | 格式 | 生成函数 |
|---|---|---|
| 私聊 | `p_<小uid>_<大uid>` | `service.PrivateConvID(a, b)` |
| 群聊 | `g_<gid>` | `service.GroupConvID(gid)` |

双方 uid 排序后拼接，保证同一对用户只有一种写法，不需要「会话表」来分配 ID。
代价是 `conv_id` 长度随 uid 位数变化，列宽留到 64。

### 5.2 seq 的分配与恢复

- 分配：`cache.NextSeq`，Lua 脚本内 `EXISTS` + `INCR`，键为 `im:seq:{convID}`。
- 键不存在时脚本返回 **0**，service 回源 `MessageRepo.MaxSeq` 查库中最大 seq，
  再调 `cache.InitSeq` 原子地把键抬到 `max(seq)` 之后继续分配。
- 为什么拆两步：若先 `INCR` 再修正，那条消息已经占用了 1 号，并发下别的请求会拿到 2，
  与库里历史 seq 重叠；先探测则不会消耗号段，所有并发请求由 `initSeqScript` 的
  `cur < floor` 条件收敛到同一段序号。
- 客户端可用 seq 是否连续判断是否丢消息；`(conv_id, seq)` 目前**没有唯一约束**，
  依赖上面这套分配逻辑保证唯一。

### 5.3 私聊写扩散 vs 群聊读扩散

| | 私聊 | 群聊 |
|---|---|---|
| 会话列表数据源 | `conversations`（双方各一行） | `groups` 的最新消息指针 + `group_members` |
| 一条消息的写入 | INSERT 1 行 + UPSERT 2 行 | INSERT 1 行 + UPDATE 1 行 |
| 未读数 | Redis `HINCRBY` | `last_msg_seq − last_read_seq` 实时算 |
| 已读位点 | `conversation_reads` | `conversation_reads`（Redis 优先） |

群未读不落存储，因此 Redis 丢失后可以按 seq 差自愈；私聊未读失真的兜底是 `conversations.last_msg_id`。

### 5.4 幂等

| 场景 | 机制 |
|---|---|
| 发消息 | Redis `SETNX im:dedup:{uid}:{clientMsgID}`（10 分钟）+ DB `uk_from_client` 兜底 |
| 点赞 | `uk_moment_user` + `ON CONFLICT DO NOTHING` |
| 已读上报 | `GREATEST(last_read_seq, ?)`，只前进 |
| 会话设置 | `ON CONFLICT DO UPDATE`，重复设置同一值为幂等 |

### 5.5 冗余字段

| 字段 | 冗余来源 | 一致性保障 |
|---|---|---|
| `groups.member_count` | `group_members` 行数 | 成员增删同事务，`LockGroup` 行锁防并发超限 |
| `groups.last_msg_*` | `messages` 最新一条 | 与消息插入同事务，`WHERE last_msg_seq < ?` 防回退 |
| `conversations.last_msg_*` | 同上 | 同上 |
| `moments.like_count` / `comment_count` | 明细表行数 | 与明细增删同事务，`GREATEST(...,0)` 防负 |

---

## 六、已知问题与待办

### 6.1 已修复

1. `messages` 未嵌入 `BaseModel`，`BeforeCreate` 兜底失效、撤回没有 `updated_at`。
   现已统一嵌入，并删除 `Send` 里手动 `snowflake.Next()` 的赋值。
2. `MaxSeq` 无调用方（死代码），同时 `NextSeq` 是裸 `INCR`，Redis 丢键后 seq 从 1 重来
   与历史重叠。现已改为「探测 + 回源初始化」，两块一起解决。
3. `moments.status` 用魔法数字 0/1，现已补 `MomentStatusNormal` / `MomentStatusDeleted`。
4. `conversations.idx_user_last` 缺 `user_id` 首列，导致会话列表排序用不上该索引，已在 tag 修正。

### 6.2 仍存在

1. **`AutoMigrate` 只加不改**。6.1 第 1 条会让存量 `messages` 表新增 `updated_at` 列，
   第 4 条则完全不会生效（同名索引已存在时 GORM 直接跳过），需要手工处理：

   ```sql
   -- 会话列表索引补上 user_id 首列；改完重启服务由 AutoMigrate 重建
   ALTER TABLE conversations DROP INDEX idx_user_last;
   ```

   删除前先 `SHOW INDEX FROM conversations;` 确认索引存在，缺失时 `DROP` 会报错。
   `messages` 数据量大时，加列请安排在低峰期，MySQL 8.0 的 `ALGORITHM=INSTANT` 可避免锁表。

2. **`(conv_id, seq)` 无唯一约束**。分配逻辑出错（例如人为删了 Redis 键、或老数据来自
   更早版本）时 DB 不会拦重复 seq。补唯一索引前要先跑一遍聚合确认存量无重复：

   ```sql
   SELECT conv_id, seq, COUNT(*) FROM messages GROUP BY conv_id, seq HAVING COUNT(*) > 1;
   ```

3. **`messages.extra` 无长度校验**。列是 `varchar(1024)`，超长在严格模式下直接报 1406，
   前端收到 500。建议在 `validateContent` 里按字节校验。
4. **`users.phone` 未启用且非唯一索引**。将来若做手机号登录，不能直接加 `uniqueIndex`：
   现有数据里 `phone` 多为空串，唯一索引会让第二条空串插入失败，需要先改成
   `NULL` 默认值再建索引。
5. **`PublicUser` 对外暴露 `username`**。好友列表、搜索结果、点赞列表都会带出对方的登录名。
   当前产品形态下等同于 QQ 号的定位，若要收敛隐私，需前端同步改 `nickname` 兜底逻辑
   （`web/index.html` 有多处 `@${username}` 展示）。
6. **`moment_comments` 物理删除，`parent_id` 可能悬空**。删一级评论后子回复仍保留，
   但父节点已不存在，前端会渲染不出上下文；`comment_count` 也只减 1，不级联。
   若要级联，需要在 `DeleteComment` 里按 `parent_id` 递归或一次性删除并扣减对应数量。
7. **每张表的 `created_at` 单列索引对大表是纯写放大**。`messages` 的查询全部带
   `conv_id`，`idx_messages_created_at` 基本不会被用到。若做容量优化，可把
   `BaseModel.CreatedAt` 的 `index` 去掉，但会影响审计类查询，需逐个确认。

### 6.3 加字段时的注意事项

- `int64` 一律带 `json:",string"`，切片用 `jsonx.Int64List`（`,string` 对 slice 不生效）。
- 布尔开关若要区分「未传」与「false」，用 `*bool`，不要用 `binding:"required"`。
- 新增 `NOT NULL` 列必须给 default，否则 AutoMigrate 对存量行会填零值或失败。
- 新增索引请显式命名并关注列顺序：等值过滤列在前，排序列在后。
