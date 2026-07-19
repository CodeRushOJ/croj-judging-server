# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) 与语义化版本。

## [Unreleased]

### Added

- 增加面向外部 OJ 的异步 REST v1 基础：RFC 9457 错误、request ID、scope 鉴权和 capabilities 端点。
- 增加 Judge 自有 MySQL schema 的嵌入式迁移、advisory lock、checksum drift 拒绝及租户隔离数据结构。
- 增加不透明 256-bit API Key 生成、peppered HMAC 存储、严格 scope 加载与 `judge-admin` 租户/密钥预置命令。
- 增加异步判题任务的提交、稳定游标列表、详情与幂等取消 REST 合约；所有响应均为脱敏视图，不返回源码、隐藏测试或对象存储信息。
- 增加持久化任务状态机，覆盖 attempt CAS、lease heartbeat/过期回收、陈旧 worker 拒绝、取消意图、基础设施重试与终态失败。
- 增加外部 OJ webhook v1 的 HMAC-SHA256 精确载荷签名、2xx/重试/永久失败矩阵及禁止重定向投递。
- 增加 callback HTTPS authority 固定、逐次 DNS 解析、混合公私网答案拒绝和 IPv4/IPv6/云元数据 SSRF 防护传输层。
- 增加 Redis 服务端时间驱动的原子令牌桶，跨 HTTP 副本统一限制租户任务提交；配额状态不确定时新任务写入 fail closed，已授权读请求保持可用。上传字节配额将在 bundle upload 分支接线后启用。
- 增加 `POST/GET /api/v1/bundles`：有界单文件 multipart 流式上传、SHA-256 内容寻址、tenant-scoped 元数据与 RFC 9457 错误。
- 增加外部题包 ZIP 安全复用校验、case CRC/size/UTF-8 流式核验、取消/超限清理、MySQL 原子 ownership+幂等事务及并发单逻辑记录回归。
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

### Changed

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

### Known limitations

- 当前每个 case 会重复编译；Issue #11 跟进 compile-once batch sandbox API。
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
