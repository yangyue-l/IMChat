# 变更日志

本文件逐条汇总仓库的变更，一条对应一个 git 提交，短 hash 可直接 `git show` 查阅。
仓库建立之前是本地开发阶段、没有提交历史，所以更早的演进无法按提交拆分。
每条只留一句话说明「改了什么」，「为什么」写在各自的提交说明里，时间信息以 git 历史为准。

分类口径与提交的 type 一致，组内按提交先后排列。

## feat

- `fee7498` 新增用户在线状态查询接口 `GET /api/v1/users/:user_id/online`，在线判定统一走 Presence
- `fba3ab7` 新增未读汇总接口 `GET /api/v1/conversations/unread`，`total` 由服务端算好
- `6ba32a1` 新增群成员分页接口 `GET /api/v1/groups/:group_id/members`
- `44b9dda` 消息支持引用，随消息下发 `quote` 摘要，并校验同会话、未撤回
- `24403a3` 撤回开放给群管理员（撤回他人消息不受时间窗限制），窗口由 `message.recall_window` 配置
- `b36eb93` 新增登录设备列表与踢线接口 `GET /api/v1/sessions`、`DELETE /api/v1/sessions/:device`
- `d569e9b` 新增会话内消息关键词检索 `GET /api/v1/messages/search`
- `860f02e` 新增下行帧丢弃计数，发送队列满时不再静默丢帧
- `ac97698` 前端拆成 Vue3 + Vite 独立工程，源码在 `web/app`，构建产物仍入库
- `ac1cb64` 新增 `media` 包：库里存对象 key，出站统一拼访问地址，换 CDN 只改配置
- `c9111d3` 新增 `storage` 包，图片写入阿里云 OSS，接口只暴露 Put / URL / Delete / Ping
- `7ba025b` 换头像、删动态、撤回图片时回收旧对象，按 key 里的 uid 判断归属
- `86c1fea` 上传接口同时回传 key 与 url，并修掉 multipart 上限少留余量的问题

## fix

- `1c1ba0b` 收紧数据模型约束，补齐消息校验与评论级联删除
- `3101ceb` 顶号不再误删在线态，在线键的余量按续期节奏留足
- `d37d8c0` 生产者补自动重连，消费者改多 worker，并修正重试计数的类型解析
- `c3ab103` 上下行帧计数拆成两个指标，避免两个方向的量混在一个数里
- `54f84e3` healthz 并发探测 MySQL 与 Redis，降级原因只进日志
- `6cd662b` 启动期校验 WS 时序与上传上限（`ws_pong_wait`、`ws_write_wait`、`upload.max_size_mb`）

## refactor

- `ad17e8e` Redis Key 统一登记到 cache 常量表，计数键补滑动 TTL
- `cad87eb` 在线判定收敛到 Presence 接口，单机与集群的差异在内部消化
- `b00af7d` 分页参数解析收敛到 `handler/common.go` 的严格 helper，非法值改报 40000
- `dc53cbc` `service/message.go` 按发送 / 读取 / 撤回拆分，纯移动不改行为

## perf

- `fd50a76` 分片推送改成一次加锁处理整批接收者，避免逐个加锁

## test

- `1d3358b` 补会话 ID 生成与解析的用例
- `bf6b743` 补统一响应出口 `pkg/resp` 的用例
- `a2483da` 补令牌签发与校验的用例，顺带用 `WithValidMethods` 钉死 HS256
- `e92cc55` 补分片连接的准入、顶号与并发用例
- `dceff96` 补配置加载与校验的用例
- `9997aad` 补本地总线与事件注册表的用例
- `24588d6` 补 `Remember` 的合并回源与防穿透用例

## docs

- `5a8bca2` 去掉文档里的具体日期与脚本引用
- `f543561` 补 Redis Key 全景文档与在线判定说明
- `bf4d94d` 补 `im:seq` 结构与在线判定的排查说明
- `dd9f7e5` 新增业务错误码说明，并把 40100 与 40101 拆成两行
- `f0ba767` 新增部署说明，覆盖配置项总表与多节点要点
- `9d8a34c` 同步接口、数据模型与部署文档到「库里存 key」的新口径

## chore

- `c0a96b5` 初始化仓库，把现有工程作为基线导入
- `0d8a35d` 补 `test` / `cover` / `fmt` 三个 make 目标
- `2d04f31` 固定换行策略：仓库与工作区统一 LF，`.gitattributes` 落地
- `f245eda` 注入版本信息并在启动日志与 `/metrics` 输出
- `0cc15ee` 移除 `scripts/` 下四个无入口的验证脚本
- `379eca0` 忽略前端依赖与环境变量，删除已废弃的本地图片目录

## ci

- `06c4481` 新增 GitHub Actions 流水线，单一 verify job 跑 build / vet / gofmt / test
