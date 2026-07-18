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

### Changed

- `JudgeService` 不再模拟 Accepted；它从 EndpointSlice 轮询调度器选择 Ready sandbox，使用题目限制执行真实源码，并通过后端内部 API 发布结果。
- 移除判题服务的 MySQL 写路径；数据库仅作为源码和题目限制的只读兼容来源，结果事务、CAS 和计数由后端统一处理。
- gRPC endpoint 连接缓存增加容量上限、空闲 TTL 和 LRU 空闲连接淘汰，避免 Pod churn 导致连接无界增长。
- `400/403/404/409` 等契约型回调响应被永久 ACK；网络错误、`401/408/425/429`、`5xx` 以及沙箱资源不足/不可用错误进入有界 RocketMQ 重试，超限转入 DLQ。
- HTTP callback 禁止自动跟随重定向，避免服务 token 被 3xx 转发至非预期地址。
- callback 文本按后端 Java UTF-16 code unit 边界截断和校验，非 BMP 字符不会导致合法 Go 载荷被后端永久拒绝。

### Known limitations

- 当前兼容请求尚未加载不可变隐藏测试包，stdin 与 expected output 为空；Accepted 只表示该次源码执行成功。
- 当前仍从 MySQL 只读加载源码和题目限制；不可变隐藏测试包将替代该兼容依赖。
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
