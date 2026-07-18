# Changelog

本仓库遵循 [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) 与语义化版本。

## [Unreleased]

### Added

- 增加与 `croj-sandbox` 一致的 `SandboxService.Execute` protobuf/gRPC 客户端。
- 为 Pod endpoint 建立可复用连接、显式 RPC deadline 与确定性关闭流程。
- 增加 bufconn、fake scheduler、失败传播和 verdict 映射测试。

### Changed

- `JudgeService` 不再模拟 Accepted；它从 EndpointSlice 轮询调度器选择 Ready sandbox，使用题目限制执行真实源码，并只在收到响应后写入结果。
- `judge_info` 改为有效 JSON，以匹配后端 Flyway schema。

### Known limitations

- 当前兼容请求尚未加载不可变隐藏测试包，stdin 与 expected output 为空；Accepted 只表示该次源码执行成功。
- 当前 RocketMQ 消息仍是裸 submission ID，结果仍直接写 MySQL，尚不具备跨副本原子领取和幂等完成；后续由 Issue #5 的版本化事件与后端 callback 替代。

## [0.0.1] - 2025-04-26

### Added

- 建立 RocketMQ 消费、GORM/MySQL 模型和首版判题服务框架。
- 提供早期任务读取、结果事务更新和 sandbox client 原型。

### Historical limitations

- 早期 client 假设不存在的 HTTP `/judge` 接口，主流程未调用它而是直接模拟 Accepted。
- 服务发现依赖 ZooKeeper，缺少并发安全调度、真实执行和系统化测试。

[Unreleased]: https://github.com/CodeRushOJ/croj-judging-server/compare/main...HEAD
[0.0.1]: https://github.com/CodeRushOJ/croj-judging-server/commits/82964c3
