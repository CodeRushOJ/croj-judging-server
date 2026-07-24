# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) 与语义化版本。

## [Unreleased]

### Added

- 外部 REST 拒绝重复或合并的 `Authorization`，任务提交严格要求 `application/json`，并使用独立 2 分钟读取截止时间与有界并发槽防止认证后 Slowloris；bundle 长上传窗口保持隔离。
- 所有可重试 bundle `503` 与 OpenAPI 一致返回 `Retry-After`；canonical `cpp` 能力描述修正为真实 Sandbox C++17，并以五语言 ID/映射契约测试锁定。
- 增加唯一 canonical language/checker registry；外部 REST 在持久化前拒绝不受 Sandbox 支持的 ID，capabilities、OpenAPI、bundle 与 compile-once gRPC 使用一致标识。
- 增加 MySQL `CURRENT_DATE` 日执行额度账本、attempt 原子预留、case 耗时安全求和、失败释放与 lease 崩溃恢复；tenant claim 使用持久公平游标和 `SKIP LOCKED`，永久不可满足的 reservation 稳定终态失败。
- 增加终态 job/加密源码两阶段 retention：有界幂等清理、tenant→job→source 锁序、持久 delete lease/retry-at、对象删除重试、独立审计和完整 schema v6 postcondition。
- 增加 HTTP header/read/write/idle timeout、bundle 上传并发上限，以及轮询 `failureCode` 合约。
- 增加独立 `external-staging/` 无主题包回收、双重 MySQL 引用核验与最长 40 分钟应用级发布 deadline；源码 reservation 和 staging 对象网络操作均移出数据库事务。

- 增加 RocketMQ 与 durable REST worker 共用的 canonical compile-once batch execution core，按顺序保留 case 结果并统一 AC/WA/CE/TLE/MLE/RE/SE 映射。
- immutable bundle manifest 增加必填时间/内存限制；tenant policy 与 capabilities 增加对应上限，上传在平台或租户 ceiling 外 fail closed。
- 增加 headless Kubernetes Service、`dns:///...` gRPC target 与 `round_robin` Pod endpoint 分配，手工 EndpointSlice 调度降级为 deprecated fallback。
- 增加完整 lease fence 的 durable execution input、heartbeat/取消控制与 worker runner；取消或 lease ownership 丢失会传播 context cancellation，陈旧 worker 不能提交结果。
- 增加默认关闭、显式 opt-in 的外部 REST/worker production runtime；readiness 同时依赖 MySQL、Redis、MinIO bucket 和 Sandbox DNS，shutdown 先停 worker 后停 HTTP。
- 增加外部 OJ 异步 REST 的 OpenAPI 3.1 契约、可复制 curl 与 Draft 接入入口；`kin-openapi` 固定版本契约测试会校验规范、真实 handler 路由/状态/header、严格 DTO examples 和公开响应脱敏。
- 增加面向外部 OJ 的异步 REST v1 基础：RFC 9457 错误、request ID、scope 鉴权和 capabilities 端点。
- 增加 Judge 自有 MySQL schema 的嵌入式迁移、advisory lock、checksum drift 拒绝及租户隔离数据结构。
- 增加不透明 256-bit API Key 生成、peppered HMAC 存储、严格 scope 加载与 `judge-admin` 租户/密钥预置命令。
- 增加异步判题任务的提交、稳定游标列表、详情与幂等取消 REST 合约；所有响应均为脱敏视图，不返回源码、隐藏测试或对象存储信息。
- 增加持久化任务状态机，覆盖 attempt CAS、lease heartbeat/过期回收、陈旧 worker 拒绝、取消意图、基础设施重试与终态失败。
- 增加外部 OJ webhook v1 的 HMAC-SHA256 精确载荷签名、2xx/重试/永久失败矩阵及禁止重定向投递。
- 增加 callback HTTPS authority 固定、逐次 DNS 解析、混合公私网答案拒绝和 IPv4/IPv6/云元数据 SSRF 防护传输层。
- 增加 operator-only `judge-admin callback create`：callback-specific 256-bit secret 只显示一次，并以带完整规范 URL AAD 和 key version 的 AES-256-GCM 保存；支持 add-before-switch key rotation，旧的不完整 callback 迁移时 fail closed 禁用。
- 增加 MySQL transactional outbox：job 的完成、终态基础设施失败及取消与稳定版本化 webhook event 原子提交，数据库 `UNIQUE(job_id)` 保证每个任务至多一个逻辑事件。
- 增加多副本 webhook delivery lease：MySQL 时钟、per-tenant head fairness、`FOR UPDATE SKIP LOCKED`、attempt/token fencing、崩溃重领及 stale settlement 拒绝；相同 `eventId`/body 按 at-least-once 语义重投。
- 增加结构化 webhook outcome、完整 HTTP retry/permanent matrix、有界 `Retry-After`、指数 jitter、12 次/24 小时默认边界、`DEAD` 审计和 30 天 terminal retention。
- 增加真实 MySQL 8.4.10 webhook 合约门禁，覆盖原子终态、lease recovery、精确 HMAC body、远端成功但 settlement 丢失后的稳定重投。
- 将 callback/source 多版本 key ring、独立 Judge DSN、schema v6 校验与多副本 webhook worker 接入生产 lifecycle；仅启用异步 REST 时可完全跳过 Backend DB、Backend callback 与 RocketMQ。
- 增加 Redis 服务端时间驱动的原子令牌桶，跨 HTTP 副本统一限制租户任务提交与 bundle 实际上传字节；配额状态不确定时新写入 fail closed，已授权读请求保持可用。
- 增加 `POST/GET /api/v1/bundles`：有界单文件 multipart 流式上传、SHA-256 内容寻址、tenant-scoped 元数据与 RFC 9457 错误。
- 增加外部题包 ZIP 安全复用校验、case CRC/size/UTF-8 流式核验、取消/超限清理、MySQL 原子 ownership+幂等事务及并发单逻辑记录回归。
- 外部题包幂等键统一采用独立 pepper 的 HMAC-SHA256；对象发布改为 durable staging→PENDING→CAS-leased PUBLISHING→远端 size/SHA 校验→READY 状态机，增加持久 reconciler、退避/attempt/lease、ABANDONED 审计、单 promoter 和客户端不重放恢复测试。
- CI 增加 digest 固定 MySQL 8.4.10 的 bundle integration job，真实运行迁移、并发、不可见窗口、reconciliation，以及 legacy pending 放弃与重新上传恢复，不允许通过 skip 绕过。
- 增加真实 MySQL 8.4 `MySQLJobRepository`：peppered-HMAC 提交幂等、canonical hash 冲突检测、租户自有 bundle/callback、queued/running quota、稳定签名游标、tenant-safe GET/取消。
- 增加 AES-256-GCM 源码对象生命周期接口、MinIO/S3 conditional-create/bounded-read 实现及事务失败补偿；worker 通过权威数据库元数据加载密文，源码、对象引用和 lease token 不进入 REST view。
- 增加 `FOR UPDATE SKIP LOCKED` worker 领取、256-bit lease token、tenant-bound attempt、heartbeat/完成 CAS、进程重启过期回收、取消恢复和有界基础设施重试。
- worker lease 改用 MySQL 权威时钟，并在并发 tenant 候选竞争后重新选择；源码上传增加 durable reservation 与孤儿清扫，租户源码上限与 64 MiB 密文边界保持一致。
- 增加持久仓储到异步 REST `JobService` 的脱敏适配，并区分配额已耗尽 `429` 与配额状态未知 `503`。
- 增加与 `croj-sandbox` 一致的 `SandboxService.Execute` protobuf/gRPC 客户端。
- 为 Pod endpoint 建立可复用连接、显式 RPC deadline 与确定性关闭流程。
- 增加 bufconn、fake scheduler、失败传播和 verdict 映射测试。
- 增加严格校验的 `SubmissionRequested` v1 RocketMQ 消息契约。
- 增加带 `X-CROJ-Service-Token` 的后端判题结果回调，支持 `APPLIED`/`DUPLICATE` 幂等完成语义。
- 增加进程内有界任务注册表，合并并发重复事件，并在临时回调失败后复用稳定结果载荷。
- 增加消息 ACK/重试分类、回调契约、并发幂等与连接淘汰测试。
- 增加 `submission.problem_version_id` 到唯一 `t_test_bundle` 的只读不可变元数据链路。
- 增加 `t_problem_version` 的发布态、题目归属和严格执行快照校验，时间/内存与 ACM/SPJ/OI 判定不再读取可变 `t_problem`。
- 增加严格 manifest v1、确定性 ZIP builder、安全 central-directory 校验和 exact/token ACM checker。
- 增加 S3/MinIO 有界流式下载、size/SHA-256 校验及原子发布。
- 增加 checksum-keyed 磁盘缓存、并发下载合并、命中校验、损坏修复、TTL/LRU 和 restart orphan 清理。
- 增加 ACM 多 case 顺序执行、选手错误早停、最大资源聚合与基础设施故障有界换 endpoint。
- 增加 fake Kubernetes API + 真实 TCP gRPC + HTTP callback 的跨组件契约测试，覆盖 EndpointSlice churn、过载换节点、bundle digest fail-closed 与 hidden payload 脱敏。
- 增加 `ExecuteBatchV1` 流式客户端和 compile-once hidden bundle pipeline；一个提交把全部有序 case 发送到一个 sandbox 编译一次。
- 增加 batch 事件顺序/终结校验、整批 EndpointSlice failover、部分流丢弃和编译诊断脱敏。

### Changed

- 取消或编译失败在缺少可信 case 计量时扣除完整日额度 reservation；只有平台基础设施失败和过期执行 lease 退款，避免客户端主动中止绕过额度。
- MySQL 1205/1213 作为可恢复竞争重试，不再导致整个 worker Pod 退出；schema v6 启动后置校验覆盖额度/attempt/source/audit 约束与索引属性。
- 终态 retention 改为短事务的 tenant→job→source `SKIP LOCKED` 领取，对象删除和上传 admission 的 Redis/MinIO I/O 全部在事务外执行。
- 源码对象 PUT 增加独立 2 分钟应用级 deadline，确保远早于 reservation lease 与孤儿回收安全窗口；callback 在事务外 admission 后由提交事务再次权威校验。
- 任务提交从认证后的 body 读取到 Redis admission、MySQL/MinIO 发布均受端到端 deadline 约束；容量拒绝立即终止未读 socket，读取超时返回可重试 `408`，关闭期与超时统一返回 RFC 9457。
- worker 在单次 claim 事务内只读取一次 MySQL 记账日期，源码读取具有独立对象存储 deadline；schema v6 同时校验关键外键的 `RESTRICT` 更新/删除动作。
- staging GC 只保护仍可能发布的 `PENDING`/`PUBLISHING` 引用，并为 List、引用查询与删除分别设置新 deadline；已过期幂等记录不再阻塞终态数据保留清理。
- `Capabilities` 初始化现在拒绝不符合 v1 公共契约的 language metadata、非正数 bundle/case limits 与超过 256 的 case count，并把未配置的 judge mode/checker 规范化为空 JSON 数组。
- `JudgeService` 不再模拟 Accepted；它从 EndpointSlice 轮询调度器选择 Ready sandbox，使用题目限制执行真实源码，并通过后端内部 API 发布结果。
- 移除判题服务的 MySQL 写路径；数据库仅用于只读提交、不可变题目版本与测试包元数据，结果事务、CAS 和计数由后端统一处理。
- gRPC endpoint 连接缓存增加容量上限、空闲 TTL 和 LRU 空闲连接淘汰，避免 Pod churn 导致连接无界增长。
- `400/403/404/409` 等契约型回调响应被永久 ACK；网络错误、`401/408/425/429`、`5xx` 以及沙箱资源不足/不可用错误进入有界 RocketMQ 重试，超限转入 DLQ。
- HTTP callback 禁止自动跟随重定向，避免服务 token 被 3xx 转发至非预期地址。
- callback 文本按后端 Java UTF-16 code unit 边界截断和校验，非 BMP 字符不会导致合法 Go 载荷被后端永久拒绝。
- 判题从单次空 stdin 兼容请求切换为 problem-version 固定的隐藏测试包；缺失、损坏、SPJ/OI 均明确发布 `SYSTEM_ERROR`。
- `Output Limit Exceeded` 在 callback v1 暂映射为 `RUNTIME_ERROR` 且不重试，等待 Issue #13 的正式枚举。
- 全部 endpoint 均返回 `Unavailable`/`ResourceExhausted` 时保留 gRPC 状态交由 RocketMQ 重试，不再错误发布终态 `SYSTEM_ERROR`。
- 隐藏包判题不再透传 sandbox 的自由文本编译诊断，防止异常 sandbox 通过 callback 回显源码或 hidden input/output。
- 隐藏包从逐 case unary RPC 切换为版本化 batch RPC；短暂过载/断连只重试完整 batch，选手终态不重试。

### Fixed

- bundle 上传默认 read/write deadline 从不兼容 512 MiB 上限的 30 秒调整为 15/20 分钟；启动时按最低 1 MiB/s、2 分钟 framing 余量和 5 分钟发布响应余量校验，防止对象已提交但客户端收到 EOF 后重试。
- 正常结束但事件畸形的 batch 流不再换 Endpoint 重编译，只将传输层 `Unavailable`/`ResourceExhausted` 视为可切换容量故障
- token checker 读取 expected 后只保留规范化 SHA-256，在 sandbox 内恢复首错早停，并在 judging 侧用同一 hash-only 规则复核
- batch 客户端显式设置有界 64 MiB 收发上限，配合 sandbox 有界编译诊断避免大响应被误判为容量重试
- batch 请求按 case 增量执行 64 MiB wire-size 检查，响应流在线限制为 `case 数 + 1` 个事件和 64 MiB 累计 protobuf 数据，且要求终结事件
- 整批 failover 记录已尝试 endpoint，EndpointSlice 更新与并发轮询时也不会在一次逻辑判题中重复选中同一 sandbox

### Known limitations

- SPJ/OI 尚未支持并返回 `SYSTEM_ERROR`；Issue #12 跟进版本化能力。
- exact checker 上线依赖 `croj-sandbox#10` 先移除 WA expected/actual 日志，避免 hidden data 泄露。
- 任务结果缓存是进程级优化，进程重启后的最终幂等由后端 result receipt 保证。

## [0.0.1] - 2025-04-26

### Added

- 建立 RocketMQ 消费、GORM/MySQL 模型和首版判题服务框架。
- 提供早期任务读取、结果事务更新和 sandbox client 原型。

### Historical limitations

- 早期 client 假设不存在的 HTTP `/judge` 接口，主流程未调用它而是直接模拟 Accepted。
- 服务发现依赖 ZooKeeper，缺少并发安全调度、真实执行和系统化测试。

[Unreleased]: https://github.com/CodeRushOJ/croj-judging-server/compare/main...HEAD
[0.0.1]: https://github.com/CodeRushOJ/croj-judging-server/commits/82964c3
