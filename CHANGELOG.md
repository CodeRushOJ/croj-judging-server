# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) 与语义化版本。

## [Unreleased]

### Added

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

- 正常结束但事件畸形的 batch 流不再换 Endpoint 重编译，只将传输层 `Unavailable`/`ResourceExhausted` 视为可切换容量故障
- token checker 使用 hash-only expected 在 sandbox 内恢复首错早停，并继续在 judging 侧用原始 expected 复核
- batch 客户端显式设置有界 64 MiB 收发上限，配合 sandbox 有界编译诊断避免大响应被误判为容量重试
- batch 请求按 case 增量执行 64 MiB wire-size 检查，响应流在线限制为 `case 数 + 1` 个事件和 32 MiB 累计 protobuf 数据，且要求终结事件
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
