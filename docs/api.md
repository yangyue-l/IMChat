# IMCHat 接口文档

统一约定：

- 基础路径：`/api/v1`
- 鉴权：`Authorization: Bearer <token>`（WebSocket 用 `?token=`）
- 请求/响应均为 `application/json`
- `int64` 类型 ID 一律**以字符串返回**，避免前端精度丢失

响应结构：

```json
{ "code": 0, "msg": "ok", "data": {} }
```

| code | 含义 |
|---|---|
| 0 | 成功 |
| 40000 | 参数不合法 |
| 40100 / 40101 | 未登录 / 令牌过期 |
| 40300 | 无权限 |
| 40400 | 资源不存在 |
| 40900 | 冲突（重复申请、用户名占用等） |
| 42900 | 请求过于频繁 |
| 50000 | 服务端错误 |

---

## 一、账号

### POST /auth/register

```json
{ "username": "alice", "password": "123456", "nickname": "爱丽丝" }
```

返回 `{ token, expires_in, user }`。用户名 3-20 位字母数字下划线，密码 6-32 位。

### POST /auth/login

```json
{ "username": "alice", "password": "123456", "device": "web" }
```

连续失败 5 次锁定 5 分钟。

### POST /auth/logout

令牌加入 Redis 黑名单，剩余有效期内不可再用。

### GET /users/me · PATCH /users/me

```json
{ "nickname": "新昵称", "avatar": "/uploads/xx.png", "gender": 1, "signature": "签名" }
```

### GET /users/search?keyword=ali&limit=20

返回 `[{ id, username, nickname, avatar, gender, signature, is_friend }]`。

### GET /users/:user_id

### GET /users/:user_id/online

返回 `{ "user_id": 1001, "online": true }`。

可见性：只能查自己或好友（先判拉黑、再判好友，与朋友圈一致），否则 403 且提示「只能查看好友的在线状态」。
非好友与不存在用户返回同一个 403，不泄露账号是否存在。

判定依据随部署模式变化（`cluster.enabled`）：单机直接读内存连接表，**精确**且零 Redis 往返；
集群模式下本节点未命中时回源 `im:online:{uid}`，受心跳续期节奏影响（续期 60s，TTL 150s），
即设备异常掉线后最长约 150s 才判离线。回源失败返回 500 而不是「离线」——
「查不到」和「不在线」必须能区分开。

---

## 二、好友

### POST /friend-requests

```json
{ "to_user_id": "2113507230744580096", "message": "你好" }
```

已是好友 / 已有待处理申请 / 存在拉黑关系时返回错误。

### GET /friend-requests?type=incoming

`type=incoming` 收到的，`outgoing` 发出的。返回项含 `status`：0 待处理、1 已同意、2 已拒绝。

### POST /friend-requests/:request_id/handle

```json
{ "accept": true }
```

同意时在一个事务内建立双向好友关系。

### GET /friends

返回 `[{ ...user, remark, starred, online, created_at }]`。

`online` 是批量算出来的：一次调用只多一次 Redis 往返（单机则纯内存），不会逐人查。
它是列表里的装饰字段，**查询失败时统一按 `false` 返回**而不是让整个列表报错 ——
要精确判断某人是否在线请用 `/users/:user_id/online`，那里失败会明确报错。

### PATCH /friends/:friend_id

```json
{ "remark": "老王", "starred": true }
```

### DELETE /friends/:friend_id

---

## 三、黑名单

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /blacklist | 黑名单列表 |
| POST | /blacklist | `{ "user_id": "..." }` 拉黑 |
| DELETE | /blacklist/:user_id | 解除拉黑 |

拉黑效果：**双向**无法发消息、无法互相查看朋友圈与动态详情、无法发起好友申请。

---

## 四、群聊

### POST /groups

```json
{ "name": "技术交流群", "member_ids": ["2113...", "2114..."] }
```

单次最多邀请 20 人，群上限 **1000 人**（`max_member` 字段随群信息返回）。

### GET /groups · GET /groups/:group_id

详情返回 `{ group, members }`，成员含 `role`（0 成员 / 1 管理员 / 2 群主）、`muted`。

`members` 是全量返回（群上限 1000 人），成员多的群请改用下面的分页接口。

### GET /groups/:group_id/members

群成员分页，仅群内成员可见（非成员返回 `40300`）。

| 查询参数 | 说明 |
|---|---|
| `offset` | 跳过条数，缺省 0 |
| `limit` | 每页条数，缺省 **50**，上限 **200**（超出按 200 处理，`0` 按缺省处理） |

```json
{
  "total": 128,
  "offset": 40,
  "limit": 20,
  "members": [
    { "id": "2113...", "nickname": "张三", "role": 2, "group_nickname": "群主本人", "muted": false, "joined_at": "..." }
  ]
}
```

排序：`role` 降序（群主 → 管理员 → 成员）→ `joined_at` 升序 → `id` 升序。
`total` 取群表冗余的 `member_count`，与群详情里的 `member_count` 一致。
`offset`/`limit` 传非整数或负数返回 `40000`，不会静默回退成第一页。

### PATCH /groups/:group_id

```json
{ "name": "新群名", "avatar": "/uploads/xx.png", "notice": "群公告" }
```

需群主或管理员。

### 成员管理

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /groups/:id/members | 成员分页（见上） |
| POST | /groups/:id/members | `{ "user_ids": [...] }` 邀请成员 |
| DELETE | /groups/:id/members/:uid | 踢人；`:uid` 为自己即退群 |
| POST | /groups/:id/mute | `{ "user_id": "...", "muted": true }` 禁言 |
| POST | /groups/:id/admin | `{ "user_id": "...", "admin": true }` 设/取消管理员 |
| POST | /groups/:id/transfer | `{ "user_id": "..." }` 转让群主 |
| DELETE | /groups/:id | 解散群（仅群主） |

权限规则：管理员可踢普通成员、群主可踢任何人；管理员之间不可互踢；群主不能直接退群。

---

## 五、消息与会话

### 会话 ID 规则

| 类型 | 格式 |
|---|---|
| 私聊 | `p_<小uid>_<大uid>` |
| 群聊 | `g_<gid>` |

### POST /messages/private

```json
{
  "to_id": "2113507230744580097",
  "msg_type": 1,
  "content": "在吗",
  "client_msg_id": "a1b2c3d4"
}
```

`msg_type`：1 文本、2 图片、3 文件。`client_msg_id` **必填**，用于幂等去重。

### POST /messages/group

```json
{ "group_id": "...", "msg_type": 1, "content": "大家好", "client_msg_id": "..." }
```

### GET /messages?conv_id=p_xxx&before_seq=120&limit=20

游标分页：传上一页最小 `seq`，返回按时间正序排列的数组。

### GET /conversations

```json
[{
  "conv_id": "p_2113_2114", "conv_type": 1, "target_id": "2114",
  "name": "鲍勃", "avatar": "", "last_msg_preview": "在吗",
  "last_msg_at": "2026-10-23T13:50:00+08:00", "unread": 2,
  "pinned": false, "muted": false
}]
```

### GET /conversations/unread

```json
{
  "total": 24,
  "items": [
    { "conv_id": "p_2113_2114", "unread": 17 },
    { "conv_id": "p_2113_2115", "unread": 5 },
    { "conv_id": "p_2113_2116", "unread": 2 }
  ]
}
```

未读汇总，`total` 已由服务端累加，客户端直接用于角标。`items` 按未读降序，
同未读按 `conv_id` 升序（顺序稳定，可直接渲染）。空结果返回 `items: []` 而不是 `null`。

统计口径：只含**私聊**会话。群未读在写扩散下不落 Redis，它等于「群最新 seq − 我的已读 seq」，
只能随会话列表实时计算，取 `GET /conversations` 各项的 `unread` 字段。
Redis 不可用时该接口返回 `total: 0` + 空列表并记 Warn，不返回错误 —— 角标丢失可以被下次自增自愈，
整个首屏报错不行。

### PATCH /conversations/:conv_id

```json
{ "pinned": true, "muted": false }
```

### POST /messages/read

```json
{ "conv_id": "p_xxx", "seq": 120 }
```

已读位置只前进不回退，同时清零 Redis 未读，并向私聊对端推送已读回执。

### POST /messages/:message_id/recall

仅发送者本人、2 分钟内有效。

---

## 六、朋友圈

### POST /moments

```json
{ "content": "今天天气不错", "images": ["/uploads/2026/10/23/xxx.jpg"], "visibility": 2 }
```

`visibility`：1 公开、2 仅好友、3 私密。图片最多 9 张。

### GET /moments/timeline?before_id=&limit=10

好友时间线，按 ID 倒序游标分页。发布时会异步写扩散到好友的 Redis 时间线。

### GET /moments/users/:user_id

查看某人动态，按可见性过滤。

### 互动

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /moments/:id/like | 点赞（幂等） |
| DELETE | /moments/:id/like | 取消点赞 |
| GET | /moments/:id/comments | 评论列表 |
| POST | /moments/:id/comments | `{ "content": "..." }` |
| DELETE | /moments/comments/:cid | 删除评论（评论者或动态作者） |
| DELETE | /moments/:id | 删除动态（仅作者） |

---

## 七、上传

### POST /upload

`multipart/form-data`，字段名 `file`。支持 jpg/png/gif/webp，默认上限 10MB。
返回 `{ "url": "/uploads/2026/10/23/xxx.png", "size": 12345 }`。

---

## 八、WebSocket

连接：`ws://host/ws?token=<jwt>&device=web`

### 上行帧

```json
{ "type": "ping" }
{ "type": "send", "data": { "conv_type": 1, "to_id": "...", "msg_type": 1, "content": "hi", "client_msg_id": "..." } }
{ "type": "read", "data": { "conv_id": "p_x_y", "seq": 12 } }
{ "type": "typing", "data": { "conv_id": "p_x_y" } }
```

### 下行帧

| type | 说明 |
|---|---|
| `message` | 新消息，data 为消息体 |
| `ack` | WS 发送通道的回执 |
| `message_recall` | 消息被撤回 |
| `read` | 对方已读 |
| `friend_request` | 收到好友申请 |
| `friend_accept` | 申请被通过 |
| `group_event` | 群事件（成员变动、资料变更、解散） |
| `notice` | 系统通知（点赞、评论） |
| `typing` | 对方正在输入 |
| `kick` | 被顶号或服务端下线 |
| `error` | 操作失败，data.msg 为原因 |
| `pong` | 心跳应答 |

心跳：客户端 25s 发包，服务端 30s 主动 ping，75s 无 pong 断开。
单连接限流：默认 20 条/秒，突发 40；单用户最多 5 个在线设备，超出握手直接 429。

---

## 五、限流策略

被限流时返回 HTTP `429` + `code=42900`，并带以下响应头：

| 头 | 说明 |
|---|---|
| `Retry-After` | 建议等待的秒数，按最早一次放行滑出窗口的时间算 |
| `X-RateLimit-Limit` | 该规则的上界（滑动窗口为次数，令牌桶为突发容量） |
| `X-RateLimit-Remaining` | 固定为 `0`（仅在拒绝时下发） |
| `X-RateLimit-Rule` | 命中的规则名：`auth` / `read` / `write` / `upload` / `ws` |

分档规则（`read` / `upload` / `ws` 单档，`auth` / `write` 双档串联，都要过才放行）：

| 规则 | 维度 | 瞬时速率闸 | 窗口总量闸 |
|---|---|---|---|
| `auth` | IP | 5 QPS，突发 10（本地） | 30 次/分钟（Redis 滑动窗口） |
| `read` | 用户 | 50 QPS，突发 100（本地） | — |
| `write` | 用户 | 10 QPS，突发 20（本地） | 120 次/分钟（Redis 滑动窗口） |
| `upload` | 用户 | 2 QPS，突发 5（本地） | —（叠加在 write 档之上） |
| `ws` | IP | 2 QPS，突发 4（本地） | — |
| WS 上行 | 连接 | 20 条/秒，突发 40（本地） | — |

两层闸的分工：**瞬时闸限制「此刻能打多快」**，防止一秒内打满整个窗口额度；
**总量闸限制「这段时间总共能用多少」**，跨实例共享，扩容不会放宽。

未登录请求按 IP 兜底；`ratelimit.distributed=false` 或 Redis 不可用时，
双档规则只剩瞬时闸，业务不会被限流组件故障掐死。
