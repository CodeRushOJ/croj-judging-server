# CodeRushOJ Judging Server

Go 判题编排服务，负责消费 `submission-topic`、读取提交快照、发现可用沙箱、执行代码并把结果幂等回调给后端。仓库正在从早期 ZooKeeper + 模拟判题原型演进为 Kubernetes 原生的真实判题控制面。

> 当前状态：Kubernetes EndpointSlice 发现、版本化消息、认证幂等结果回调和不可变 ACM 隐藏测试包链路已经接通。上线 exact checker 前必须先合入 [`croj-sandbox#10`](https://github.com/CodeRushOJ/croj-sandbox/issues/10) 的日志脱敏修复，否则旧 sandbox 会把 WA 的 expected/actual 写入 Pod 日志。

面向外部 OJ 的版本化异步 REST 适配器正在 Draft PR #24 中实施。已有切片包括 RFC 9457 错误、请求 ID、不透明 API Key 的 peppered HMAC 验证与 scope、`GET /api/v1/capabilities`、Judge 自有 MySQL 迁移、租户/密钥预置、不可变 hidden bundle 上传/元数据端点，以及 `POST /api/v1/judge-jobs`、任务列表/详情/取消的脱敏 HTTP 合约和 lease/attempt 状态机。Webhook 已具备精确载荷 HMAC 签名、禁止重定向、状态重试矩阵及 DNS rebinding/私网 SSRF 防护；Redis 原子令牌桶使用服务端时间，在多副本间统一限制任务提交和 bundle 实际上传字节，配额不可用时新写入 fail closed 为 `503`，读取继续可用。MySQL job/outbox repository、delivery worker、key-ring rotation、生产 runtime wiring 和 E2E 门禁已经完成。

## 外部 OJ 接入（Draft）

从 [`api/openapi.yaml`](api/openapi.yaml) 获取可被工具直接加载和校验的 OpenAPI 3.1 契约。推荐接入顺序是：读取 capabilities → 上传 immutable bundle → 幂等提交 judge job → 按 `Location`/`statusUrl` 轮询，必要时请求取消。规范内包含可复制的 curl、RFC 9457 错误、租户隔离 `404`、幂等 replay/conflict、状态语义和 webhook v1 签名 framing。

此契约仍处于 Draft/beta。外部 listener 默认关闭；只有完成 schema migration、Secret/依赖接线并显式设置 `EXTERNAL_API_ENABLED=true` 才会启动 REST、durable job/outbox worker 与健康检查。未启用时不暴露外部端口。

外部 v1 的语言 ID 与 Sandbox compile-once 协议共用一个注册表：`go`、`cpp`、`python`、`java`、`javascript`；checker 只使用 bundle manifest 接受的小写 `exact`、`token`。服务会在创建源码对象和 MySQL job 前拒绝其他 ID，客户端不得把显示名称或编译器版本当作 `language`。

## 架构

```mermaid
flowchart LR
    MQ["RocketMQ submission-topic"] --> Judge["Judging Server"]
    Judge -->|"只读 immutable metadata"| DB["MySQL submission/problem_version/test_bundle"]
    Judge --> Cache["SHA-256 disk cache"]
    Cache --> MinIO["S3 / MinIO immutable ZIP"]
    Judge --> API["Kubernetes API"]
    API --> ES["EndpointSlice for croj-sandbox"]
    ES --> Scheduler["Ready endpoint cache + round robin"]
    Scheduler --> GRPC["Bounded reusable gRPC connections"]
    GRPC --> Sandbox["SandboxService.ExecuteBatchV1"]
    Sandbox --> Judge
    Judge -->|"X-CROJ-Service-Token"| Backend["Backend /api/internal/v1/judge-results"]
    Backend -->|"事务 CAS + result receipt"| DB
```

发现器只读取带 `kubernetes.io/service-name=croj-sandbox` 标签的 EndpointSlice，只保留 `Ready=true` 且非 `Terminating` 的 TCP 地址。Kubernetes API 暂时失败时，调度器保留最后一次成功快照；API 成功返回空集合时立即停止分配，避免继续调用已删除 Pod。

每个 endpoint 复用一个 gRPC `ClientConn`；连接缓存同时受最大容量和空闲 TTL 约束。每个 case 的 `Unavailable`、`ResourceExhausted`、`Sandbox Error` 或未知状态会在有界次数内换下一个 Ready endpoint。若全部尝试都是 `Unavailable`/`ResourceExhausted`，服务保留原 gRPC code 并交给 RocketMQ 重试，不会把短暂过载发布成终态 `SYSTEM_ERROR`；sandbox 已返回但内容为空、状态未知或为 `Sandbox Error` 时才按损坏的基础设施响应终结。CE/WA/TLE/MLE/RE/OLE 等选手终态不重试。OLE 在 callback v1 暂映射为 `RUNTIME_ERROR`，正式枚举由 Issue #13 跟进。

消息必须是严格的 `SubmissionRequested` v1 JSON：

```json
{"schemaVersion":1,"eventId":"50f75fdf-fdea-473f-a156-bf1ed60acf58","submissionId":99,"attemptNo":1,"problemId":42,"userId":7,"language":"java17"}
```

未知字段、非 UUID `eventId`、不支持的版本和非法标识会被永久拒绝并 ACK。进程内任务注册表按 `eventId/submissionId/attemptNo` 合并并发重复消息；回调临时失败时复用完全相同的结果，`eventId` 直接作为稳定 `resultId`。后端返回 `200 APPLIED/DUPLICATE` 时完成，`400/403/404/409` 等契约错误视为永久结果并 ACK；网络错误、`401/408/425/429` 和 `5xx` 重试。RocketMQ 重试超过配置上限后投递到 consumer group 的 DLQ，后端 result receipt 是跨进程、跨副本的最终幂等权威。

判题服务不再直接写 MySQL。它只读加载提交源码、`submission.problem_version_id` 指定的唯一 `t_problem_version` 和对应 `t_test_bundle`，建议使用只读数据库账号。执行限制与判题模式只来自不可变版本的 `limits_json` / `judge_config_json`；版本 ID、题目 ID 必须与提交一致且状态必须为 `PUBLISHED`。可变 `t_problem` 不参与主判题链。

## 隐藏测试包 v1

对象必须是确定性 ZIP，必须包含根目录 `manifest.json`。ZIP 全文件 SHA-256、压缩大小和 `manifest.json` 规范结构必须分别与 `t_test_bundle.sha256`、`size_bytes`、`manifest_json` 一致；任何一项不一致都返回 `SYSTEM_ERROR`，不会选择其中一份覆盖另一份。

```json
{
  "schemaVersion": 1,
  "judgeMode": "ACM",
  "checker": "exact",
  "cases": [
    {"id": "case-01", "input": "cases/01.in", "output": "cases/01.out", "weight": 1}
  ]
}
```

仅支持 `ACM` 的 `exact` 与 `token`。SPJ/OI 会明确返回 `SYSTEM_ERROR`，由 Issue #12 跟进。`exact` 与 sandbox 保持相同规则：CRLF/CR 统一为 LF，每行 `TrimSpace` 后再移除整体首尾空白；`token` 在 judging 侧按 Unicode whitespace 分词比较。token expected 在读取时立即规范化为长度前缀 token 序列的 SHA-256，批次结构只保留 digest。sandbox 用同一 hash 在首个 token WA 时早停，judging 收到结果后再对 actual 做同样的 hash-only 复核。一个提交把有序 case 作为单个 `ExecuteBatchV1` 请求发送到同一 sandbox，在一个私有执行生命周期中只编译一次；每个 case 仍启动独立受限进程。首个选手错误早停，最终时间/内存取所有已完成 case 的最大值。

批量流严格校验 case ID/顺序、已知状态、编译事件和最终完成事件。v1 每批最多 256 个 case，protobuf 请求最多 64 MiB；请求按 case 增量校验 wire size，超限会停止读取后续测试数据，并在 RPC 前确定性返回 `SYSTEM_ERROR`。客户端在接收过程中限制最多 `case 数 + 1` 个事件和 64 MiB 累计 protobuf 响应，这可容纳 256 个 case 同时达到默认 stdout/stderr 上限及协议开销，但仍保持硬上限。缺失终结事件或超限时立即取消并丢弃全部部分结果。只有 `Unavailable`/`ResourceExhausted` 会丢弃不完整流并在本次尚未尝试的 Ready Endpoint 上有界重试完整 batch；正常结束但畸形的事件流直接确定性 `SYSTEM_ERROR`，不重新编译。选手终态不重试。sandbox PR 必须先于 judging-server 部署，回滚顺序相反。旧 unary `Execute` 客户端仍保留用于兼容，但隐藏测试主链路不再调用它。

`SANDBOX_EXECUTE_TIMEOUT` 是单 case/编译与传输的基础预算；batch deadline 在此基础上按额外 case 的题目时间限制线性扩展，同时仍受上游 context 取消约束，避免把旧 unary 的 60 秒总 deadline 错用于整批评测。

隐藏包路径把 sandbox 视为不可信边界：callback 不转发 stdout、hidden input/output 或 sandbox 诊断。编译错误仅返回固定的 `compilation failed; diagnostics redacted`；后续若要恢复安全的编译诊断，必须由独立的结构化编译阶段提供，不允许从已接触 hidden case 的响应中透传自由文本。

读取 ZIP 前会拒绝未知字段、重复 ID/路径、绝对路径、反斜杠、路径穿越、符号链接、非普通文件、加密/未知压缩方法、zip bomb、超量文件和解压大小越界。文件不会解压到目录。`WriteDeterministicArchive` 可生成固定时间戳、固定权限、排序 entry 的可复现 artifact，并返回应写入数据库的规范 manifest JSON。

缓存以 SHA-256 为键，下载时流式执行 size/SHA-256 校验并原子 rename；并发 miss 合并为一次下载，命中会重新校验，损坏后自动重拉。同一 SHA-256 可跨不同 object key 复用完全相同的字节，但每一条数据库元数据的期望 size/SHA-256 都会独立复核；object key 仅是下载定位符。进程启动会删除本 cache 目录中的旧 ZIP、临时文件和同名 symlink，不跟随链接；因此该目录必须是 judging 专用 emptyDir。

## 技术基线

- Go 1.26
- Kubernetes/client-go 0.36.2（对应 Kubernetes 1.36）
- RocketMQ Go Client 2.1.2
- GORM + MySQL（只读兼容查询）
- MinIO Go SDK（S3-compatible immutable bundle 读取）
- go-redis v9（Redis 服务端时间 + Lua 原子令牌桶，多副本写入配额）
- Kubernetes Service、EndpointSlice 与 namespace 级最小权限 RBAC

不再依赖 ZooKeeper。

## 配置

默认配置位于 `configs/config.yaml`，适配 `coderushoj` namespace 中的平台服务。Secret 不写入文件，部署时必须通过环境变量或 Kubernetes Secret 注入数据库凭据和不少于 32 字节的 `JUDGE_RESULT_SERVICE_TOKEN`。该 token 必须和后端使用同一个 Secret。

常用覆盖变量：

| 环境变量 | 用途 | 默认来源 |
| --- | --- | --- |
| `DATABASE_HOST` / `DATABASE_PORT` | MySQL 地址 | YAML |
| `DATABASE_USERNAME` / `DATABASE_PASSWORD` / `DATABASE_NAME` | MySQL 只读凭据和库名 | YAML / Secret |
| `ROCKETMQ_NAME_SERVER` | NameServer 地址 | YAML |
| `SUBMISSION_TOPIC` / `ROCKETMQ_CONSUMER_GROUP` | 消费主题和消费组 | YAML |
| `ROCKETMQ_MAX_RECONSUME_TIMES` | 临时失败最大重试次数，超限进入 `%DLQ%<consumer-group>` | YAML |
| `BACKEND_INTERNAL_URL` | 后端内部地址，必须包含 `/api` | YAML |
| `JUDGE_RESULT_SERVICE_TOKEN` | 判题结果回调共享密钥，至少 32 字节 | Secret（必填） |
| `JUDGE_RESULT_CALLBACK_TIMEOUT` | 单次 HTTP 回调 timeout | YAML |
| `JUDGE_TASK_CACHE_CAPACITY` / `JUDGE_TASK_CACHE_TTL` | 进程内幂等任务表容量与完成项 TTL | YAML |
| `OBJECT_STORAGE_ENDPOINT` / `OBJECT_STORAGE_BUCKET` / `OBJECT_STORAGE_REGION` / `OBJECT_STORAGE_USE_TLS` | S3/MinIO 只读对象存储，endpoint 为 `host[:port]`，不含 scheme/path | YAML |
| `OBJECT_STORAGE_ACCESS_KEY` / `OBJECT_STORAGE_SECRET_KEY` | S3/MinIO 只读凭据 | Secret（必填） |
| `JUDGE_BUNDLE_CACHE_DIR` / `JUDGE_BUNDLE_CACHE_MAX_BYTES` / `JUDGE_BUNDLE_CACHE_TTL` | 专用 emptyDir 路径、容量与 TTL | YAML / emptyDir |
| `JUDGE_BUNDLE_MAX_OBJECT_BYTES` | ZIP 压缩体积上限 | YAML |
| `JUDGE_BUNDLE_MAX_FILES` / `JUDGE_BUNDLE_MAX_MANIFEST_BYTES` / `JUDGE_BUNDLE_MAX_CASE_BYTES` | ZIP 文件数、manifest、单 case 文件限制 | YAML |
| `JUDGE_BUNDLE_MAX_UNCOMPRESSED_BYTES` / `JUDGE_BUNDLE_MAX_COMPRESSION_RATIO` | zip bomb 防护限制 | YAML |
| `JUDGE_BUNDLE_MAX_INFRA_ATTEMPTS` | 每 case 基础设施故障换 endpoint 上限 | YAML |
| `SANDBOX_NAMESPACE` / `SANDBOX_SERVICE` / `SANDBOX_PORT_NAME` | EndpointSlice 选择目标（默认 gRPC 端口名 `grpc`） | YAML |
| `SANDBOX_REFRESH_INTERVAL` | 刷新周期，如 `5s` | YAML |
| `SANDBOX_EXECUTE_TIMEOUT` | 单次 gRPC Execute 的总 deadline，如 `60s` | YAML |
| `SANDBOX_MAX_CONNECTIONS` / `SANDBOX_CONNECTION_IDLE_TTL` | gRPC endpoint 连接缓存容量和空闲回收时间 | YAML |
| `KUBECONFIG` | 集群外开发时的 kubeconfig 路径 | client-go 默认规则 |

集群内优先使用 ServiceAccount token；集群外自动使用 `KUBECONFIG` 或 `$HOME/.kube/config`。

## 本地测试

宿主机无需安装 Go：

```bash
docker run --rm \
  -v "$PWD:/workspace" \
  -v coderushoj-go-cache:/go/pkg/mod \
  -w /workspace golang:1.26.3 \
  go test -race ./...
```

`internal/integration/judge_sandbox_contract_test.go` 使用一次性的 fake Kubernetes API、真实 TCP gRPC server 和 HTTP callback 验证完整进程内契约：EndpointSlice churn、过载换节点、不可变 ZIP/SHA-256、隐藏 case 执行和 callback 脱敏。测试不要求也不会启动持久集群或服务。

静态检查和构建：

```bash
docker run --rm \
  -v "$PWD:/workspace" \
  -v coderushoj-go-cache:/go/pkg/mod \
  -w /workspace golang:1.26.3 \
  sh -c 'go vet ./... && CGO_ENABLED=0 go build -trimpath -o /tmp/judging-server ./cmd'
```

## 外部 OJ 租户预置（开发中）

`judge-admin` 使用与运行时相同的不可变迁移和仓储，新 API Key 只在标准输出显示一次；MySQL 只保留 lookup prefix 和 `HMAC-SHA256(pepper, full-key)`。不要把 DSN、pepper 或输出的 key 写入仓库、shell history 或日志。

```bash
export JUDGE_DATABASE_DSN='judge_admin:...@tcp(127.0.0.1:3306)/coderushoj_judge?parseTime=true&charset=utf8mb4'
export JUDGE_API_KEY_PEPPER_B64="$(openssl rand -base64 32)"

# 每次发布新版本前先执行；命令会加 advisory lock，并严格验证 v1-v6 名称与 checksum。
go run ./cmd/judge-admin schema migrate

go run ./cmd/judge-admin tenant create \
  --name 'Example OJ' --max-queued 100 --max-running 4 \
  --max-source-bytes 1048576 --max-bundles 200 \
  --daily-execution-ms 3600000 --max-infra-tries 3

go run ./cmd/judge-admin api-key create \
  --tenant '<26-character-tenant-id>' \
  --scopes 'capabilities:read,bundle:write,bundle:read,job:submit,job:read,job:cancel'
```

在 Kubernetes 中，DSN 和 pepper 必须来自 Secret；上面的 `export` 只是本机演示。正式 rollout 应将 [`deploy/judge-schema-migration-job.yaml`](deploy/judge-schema-migration-job.yaml) 的镜像替换为与业务 Pod 完全相同的 immutable digest，并使用 `coderushoj-judge-database/dsn` Secret 先执行 `/app/judge-admin schema migrate`，成功后再启动 `/app/judging-server`。迁移通过 MySQL advisory lock 串行化并验证已发布迁移的 SHA-256；不会修改 Backend 的 Flyway history。应用启动和 `/readyz` 都只验证完整 schema，不会在业务 Pod 中隐式修改数据库。

### 外部 OJ durable webhook

Callback 只能由运维 CLI 创建，没有公开的 callback 管理 API。URL 必须是公网 DNS 名称的绝对 HTTPS URL；创建时和每次连接时都会拒绝私网、loopback、link-local、文档地址、metadata 类地址以及混合公私网 DNS 结果，且投递不跟随重定向。

```bash
export JUDGE_DATABASE_DSN='judge_admin:...@tcp(127.0.0.1:3306)/coderushoj_judge?parseTime=true&loc=UTC&charset=utf8mb4'
export JUDGE_CALLBACK_KEY_VERSION='1'
# 文件必须只允许当前用户读取，内容形如 {"1":"<base64-encoded-32-byte-AES-key>"}
export JUDGE_CALLBACK_KEYS_JSON="$(< /path/to/root-only/judge-callback-keys.json)"

go run ./cmd/judge-admin callback create \
  --tenant '<26-character-tenant-id>' \
  --url 'https://oj.example.com/webhooks/coderushoj'
```

命令只显示一次 `callbackId` 和 `croj_whsec_...` secret；应立即写入接收方的 Secret 管理系统，不要进入 Git、Issue、日志或 shell history。MySQL 只保存 AES-256-GCM 密文、12-byte nonce 和 key version，AAD 绑定 tenant、callback、key version 以及完整规范 URL（scheme/host/effective port/path/query）。轮换采用 add-before-switch：先部署同时包含新旧版本的 key ring，再切换 active version；确认没有行引用旧版本后才能移除旧 key。schema v6 会自动禁用缺 nonce 或密文元数据不完整的旧 callback，必须重新创建，绝不会伪造 secret。

任务进入 `SUCCEEDED`、`FAILED` 或 `CANCELLED` 时，job 终态与唯一 outbox event 在同一个 InnoDB 事务提交。`WebhookWorker` 使用 MySQL 时钟、`FOR UPDATE SKIP LOCKED`、attempt 和 256-bit lease token 多副本领取；HTTP 请求发生在事务外。远端已接受但 settlement 未提交时，同一 `eventId` 和完全相同的 body 会在 lease 过期后再次投递，因此接收方必须按 `eventId` 持久去重。生产 runtime 为每个副本构造独立 worker/transport cache，并在启动时校验 callback key ring 与完整 schema v6。

```mermaid
flowchart LR
    Job["Terminal job transaction"] --> Outbox["MySQL immutable outbox"]
    Outbox --> Claim["Fenced lease claim"]
    Claim --> HTTPS["HTTPS + DNS revalidation + HMAC v1"]
    HTTPS --> Receiver["External OJ receiver"]
    Receiver -->|"2xx"| Delivered["DELIVERED audit"]
    Receiver -->|"408 / 425 / 429 / 5xx / network"| Retry["PENDING with backoff"]
    Receiver -->|"3xx / other 4xx / unsafe"| Dead["DEAD audit"]
    Retry --> Claim
```

请求头：

- `X-CodeRushOJ-Event-Id`: body 中的稳定 `eventId`；
- `X-CodeRushOJ-Timestamp`: 签名时的 UTC Unix 秒；
- `X-CodeRushOJ-Signature`: `v1=<lowercase-hex-HMAC-SHA256>`。

HMAC 的精确输入是 `v1\n<eventId字节长度>\n<eventId>\n<timestamp>\n` 后直接拼接原始 body bytes。接收方必须用保存的 callback-specific secret 重新计算 HMAC、constant-time 比较、校验时间窗口，再按 `eventId` 幂等处理；不要重新序列化 JSON 后验签。

body 的 `schemaVersion` 为 `1`，事件类型为 `judge.job.completed`、`judge.job.failed` 或 `judge.job.cancelled`。通用字段是 `eventId`、`eventType`、`occurredAt`、`tenantId`、`jobId`、必填 `status` 和可选 `clientReference`；成功事件包含脱敏 `result`，失败事件只包含稳定 `failureCode`，取消事件不包含两者。源码、hidden case、对象 key、worker/lease 和 callback secret 永不进入 body。

所有 `2xx` 成功；`408`、`425`、`429`、`5xx` 和网络故障重试；`1xx` 终态响应按 `invalid_delivery` 处理，`3xx`、其余 `4xx`、SSRF/authority 拒绝及解密失败进入 `DEAD`。指数退避使用 `[0.5,1.5]` jitter，`Retry-After` 与最终延迟均硬限制为 15 分钟；默认最多 12 次、投递窗口 24 小时（最大可配置 7 天）。`DELIVERED`/`DEAD` 默认保留 30 天用于审计和去重，清理器不会删除 `PENDING`/`DELIVERING`。

CI 使用按 digest 固定的 MySQL 8.4.10 service 执行真实 race 合约测试。下面的脚本只静态验证 workflow 中的镜像、DSN、超时和 focused selector 位于同一个 job/step，并运行防误报 mutation test；它不会启动数据库：

```bash
scripts/ci/test-webhook-mysql84-contract.sh
scripts/ci/test-webhook-mysql84-contract-self-test.sh
```

本机可直接启动带健康检查和独立端口的一次性 MySQL，再运行同一 selector（需要本机 Go 1.26.3）：

```bash
docker run -d --name croj-webhook-mysql84 -p 33061:3306 \
  --health-cmd='mysqladmin ping -h 127.0.0.1 -uroot --silent' \
  --health-interval=2s --health-timeout=5s --health-retries=30 \
  -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -e MYSQL_DATABASE=croj_webhook_test mysql:8.4.10
until [ "$(docker inspect -f '{{.State.Health.Status}}' croj-webhook-mysql84)" = healthy ]; do sleep 2; done
export JUDGE_TEST_MYSQL_DSN='root:@tcp(127.0.0.1:33061)/croj_webhook_test?parseTime=true&loc=UTC&charset=utf8mb4'
go test -race -count=1 -timeout=10m ./internal/external -run '^TestMySQLWebhook'
docker rm -f croj-webhook-mysql84
unset JUDGE_TEST_MYSQL_DSN
```

不要把生产数据库 DSN 用于集成测试；测试中断时也应执行 `docker rm -f croj-webhook-mysql84` 清理容器。

### 外部不可变题包上传

`POST /api/v1/bundles` 只接受一个名为 `bundle` 的 multipart 文件和 16–128 字节可见 ASCII `Idempotency-Key`。HTTP 外层和 Service 内层分别执行请求体/文件体积上限；文件流式写入专用临时文件并同步计算 SHA-256，取消、超限或校验失败都会清理临时文件。安全校验与内部 immutable bundle 共用同一条 ZIP 路径，覆盖路径穿越、link/非普通文件、加密/未知压缩、文件数、单文件/总解压量、压缩比、manifest 严格 schema、case 成对和 256 case 协议上限；每个 case 文件还会流式读取以核对 CRC、声明大小和 UTF-8，损坏内容不会发布。

HTTP 层先用可信 `Content-Length` 对 multipart 总上限做无读取早拒绝；Service 流式落临时文件后，以实际 bundle 字节数执行一次租户 Redis 配额 admission。配额拒绝返回带 `Retry-After` 的 `429`，Redis 状态不可确认返回 `503`；两者都发生在对象 staging 和 MySQL ownership 之前，不会留下持久化半成品。

服务端只能发布到 `external/<tenant-id>/sha256/<lowercase-sha256>.zip`；请求不接受 URL、bucket 或 object key，响应也只返回 `bundleId`/digest/size/case/manifest version/创建时间。`Idempotency-Key` 使用不少于 32 字节独立 pepper 的 `HMAC-SHA256` 后才进入 MySQL，与 job API 使用同一安全语义，原始 key 不落库。

通过校验的字节先写入唯一的 `external/<tenant-id>/staging/<upload-id>/<sha256>.zip`，再提交 `PENDING` ownership。发布者必须通过 MySQL CAS 取得带过期时间的 `PUBLISHING` lease；同一 bundle 同时只有一个 promoter，其他请求返回带 `Retry-After` 的 `503` 或读取已经完成的 READY 结果。promoter 从 staging 原子复制到 final key，并通过远端 HEAD 同时核对 size 与 `x-amz-meta-sha256` 后，才可把状态改为 READY。数据库所有多行写路径固定使用 tenant→bundle 锁序；对象操作被限制在 lease 的前半段，过期 claim 不能提交 READY。

`BundleReconciler` 会持久领取到期的 PENDING/过期 PUBLISHING 行，记录 attempt、next-attempt、last-error 与 lease；客户端断线或不重放时仍可完成发布。超过最大失败次数或旧迁移留下的无 staging 行会转为 ABANDONED 并保留审计信息，绝不会无条件 backfill READY；相同内容之后重新上传时会把新 staging 对象安全挂回原 bundle，并从 PENDING 重新发布。对象存储应再为 `external/*/staging/` 配置大于数据库最大重试窗口的生命周期规则，清理数据库提交失败后无法安全判定 ownership 的唯一 staging 对象。`GET` 和判题任务 lookup 只允许 `publication_status=READY AND ready_at IS NOT NULL`；跨租户统一返回 `404`。

本地未提供 DSN 时 MySQL 集成回归会跳过；对开发专用库设置 `EXTERNAL_JUDGE_MYSQL_TEST_DSN` 后，`go test -race ./internal/integration -run ExternalBundleSQLRepositoryIntegration` 会执行迁移，并真实竞争同 key/同 hash、不同 key/同 hash、同 key/不同 hash 三种事务，断言单 bundle、单 promoter、正确幂等记录数和单一可见对象；还覆盖阻塞发布期间 `404`、客户端不重放的 durable reconciliation、失败退避及 legacy pending 的放弃与重新上传恢复。GitHub Actions 的 `mysql84-bundle-integration` job 使用 digest 固定的 MySQL 8.4.10，因此该测试在 PR 中不得 skip。

### 异步任务持久化与 worker 恢复

Judge 自有 schema v6 依次提供 job/attempt 256-bit lease token、租户执行上限补全和 durable webhook outbox；attempt 通过 `(job_id, tenant_id)` 复合外键绑定到租户。`MySQLJobRepository` 在同一个 InnoDB admission 事务中锁定租户策略、校验 READY 且租户自有的 bundle/callback、确认 queued quota、写入 peppered-HMAC 幂等记录以及加密源码元数据。同键同 canonical hash 返回原 job；同键不同请求返回 `409`。只有确认是新 job 后才调用一次 Redis admission，并发同键只扣一次；同 hash replay 即使 Redis 暂时不可用仍返回原 job。已确认的队列配额耗尽返回 `429`，策略或数据库状态无法确认时返回 `503`，不会开放式接收新任务。

源码先使用 AES-256-GCM 加密，tenant ID、source ID 和 key version 作为 AAD；MySQL 仅保存 digest、长度、nonce、key version 和不可公开的对象引用。明文策略上限为 `64 MiB - 16 bytes`，为 GCM tag 预留空间并与对象传输硬上限一致。对象读写由 `SourceObjectStore` 抽象提供；MinIO/S3 实现以 `If-None-Match: *` 原子创建，拒绝随机 ID 碰撞覆盖，并按数据库密文长度有界读取。每次上传前先提交带 owner token/lease 的 durable reservation，admission 事务会锁住它并在发布 metadata/job 时原子删除；明确回滚会立即补偿删除，`COMMIT`/对象写入结果不确定时由生产 runtime 中有界运行的 reservation sweeper 在 lease 与安全窗口都过期后对照权威 source metadata 清除孤儿，已引用或仍被 admission 锁住的对象绝不删除。worker 读取源码前会用 job ID、attempt、worker ID、lease token 和未过期 lease 回查 MySQL 的权威元数据，不信任内存 claim 携带的 object key。

worker 按“最久未服务 tenant”领取并使用 `FOR UPDATE SKIP LOCKED`，锁序固定为 tenant → job → daily ledger/attempt；多副本会跳过已锁 tenant，额度不足的 deferral 也推进公平游标，不会同时挤在单一 backlog。每次领取创建单独 attempt，并按 bundle 的 `timeLimitMillis × caseCount` 在 `t_external_execution_daily` 以 MySQL `CURRENT_DATE` 原子预留 `dailyExecutionMillis`；成功以全部已执行 case 耗时的溢出安全总和结算并封顶于 reservation，取消、基础设施失败和过期 lease 释放，崩溃重领不会重复占额。策略下调后永远无法容纳 reservation 的任务会以 `DAILY_EXECUTION_LIMIT_TOO_LOW` 终态失败。lease 的签发、过期判断和 CAS 均以 MySQL 时钟为准，不受副本系统时钟偏差影响；heartbeat、完成和基础设施失败均以 attempt/worker/lease token 做 CAS。进程重启后只会回收过期 attempt，旧 worker 无法覆盖新结果；已请求取消的过期任务直接恢复为 `CANCELLED`，不会再次执行源码。基础设施失败按 tenant policy 有界重试，耗尽后才进入 `FAILED`。

外部 REST 与 durable worker 已接入同一个 compile-once `BatchBundlePipeline`，不会维护第二套判题实现。immutable bundle manifest 的 `limits.timeLimitMillis` / `limits.memoryLimitMiB` 是每题权威值；tenant policy 与 capabilities 只提供租户/平台上限。worker 通过完整 attempt/worker/token/未过期 lease fence 加载源码与 READY bundle，heartbeat、取消和完成仍由 MySQL CAS 最终裁决；旧 lease 不能写入结果。

外部端口默认关闭。只有显式设置 `EXTERNAL_API_ENABLED=true` 才会构造鉴权、Redis quota、MinIO source/bundle store、REST listener、bundle reconciler、判题 worker、retention worker 与 webhook worker。启用时必须提供独立的 `JUDGE_DATABASE_DSN`，以及 32-byte base64 的 `EXTERNAL_API_AUTH_PEPPER_BASE64`、`EXTERNAL_IDEMPOTENCY_PEPPER_BASE64`、`EXTERNAL_CURSOR_KEY_BASE64`；源码密钥使用 `EXTERNAL_SOURCE_KEY_VERSION` + `EXTERNAL_SOURCE_KEYS_JSON`，callback 密钥使用 `JUDGE_CALLBACK_KEY_VERSION` + `JUDGE_CALLBACK_KEYS_JSON`，均按 add-before-switch 保留历史解密版本。仅部署异步 REST 时设置 `LEGACY_JUDGE_ENABLED=false`，进程不会连接 Backend DB、Backend callback 或 RocketMQ。HTTP 明确限制 header/read/write/idle 时间并用非阻塞 semaphore 限制 bundle 上传并发。过期幂等记录由独立 worker 分批清理；终态 job 默认保留 30 天，只有 webhook/outbox 与幂等引用都已清理后，retention worker 才按 tenant → job → source 锁序取得持久 delete lease，事务外删除对象，再在 fence token 下删除 attempt/job/source 元数据并保留审计；其他 Pod 只能在 lease 和 retry-at 过期后接管，对象失败会记录稳定错误码并重试。`GET /livez` 只表示进程存活；`GET /readyz` 仅在 Judge schema v6 checksum、MySQL、Redis、MinIO bucket 与 Sandbox headless-Service DNS 全部可用时返回 `204`。关闭会取消在途 worker；未 settlement 的任务和 webhook 依靠 fenced lease 安全重领，然后再关闭 HTTP。

新增运行参数为 `EXTERNAL_API_READ_HEADER_TIMEOUT`、`EXTERNAL_API_READ_TIMEOUT`、`EXTERNAL_API_WRITE_TIMEOUT`、`EXTERNAL_API_IDLE_TIMEOUT`、`EXTERNAL_BUNDLE_MIN_UPLOAD_BYTES_PER_SECOND`、`EXTERNAL_BUNDLE_UPLOAD_CONCURRENCY`、`EXTERNAL_SOURCE_RETENTION`、`EXTERNAL_RETENTION_IDLE_DELAY`、`EXTERNAL_RETENTION_DELETE_TIMEOUT`；默认值和可复制部署步骤见 [`docs/operations/external-rest.md`](docs/operations/external-rest.md)。默认上传契约支持 512 MiB 测试包以不低于 1 MiB/s 上传：完整请求读取窗口为 15 分钟，写窗口为 20 分钟，其中至少 5 分钟保留给对象存储发布和最终响应；不满足这个关系的配置会在启动时失败，避免对象已经提交但客户端收到 EOF 后形成重试风暴。

Sandbox 的推荐目标是 `dns:///sandbox-workers.<namespace>.svc.cluster.local:50051`，对应 `deploy/sandbox-headless-service.yaml` 中 `clusterIP: None` 的 Service。gRPC channel 使用 `round_robin` 对 DNS 返回的 Pod endpoint 做每 RPC 分配。直接读取 EndpointSlice 的旧调度路径仅作为未配置 `SANDBOX_GRPC_TARGET` 时的 deprecated fallback。

真实 MySQL 8.4 验证使用一次性容器，不需要启动整套 OJ：

```bash
docker run --rm -d --name croj-job-mysql \
  -e MYSQL_ROOT_PASSWORD=test-root \
  -e MYSQL_DATABASE=judge_test \
  -e MYSQL_USER=judge -e MYSQL_PASSWORD=judge-test \
  mysql:8.4.10

docker run --rm --link croj-job-mysql:mysql \
  -e JUDGE_TEST_MYSQL_DSN='judge:judge-test@tcp(mysql:3306)/judge_test?parseTime=true&loc=UTC&charset=utf8mb4' \
  -v "$PWD:/workspace" -w /workspace golang:1.26.3 \
  go test -race ./internal/external

docker stop croj-job-mysql
```

## 构建镜像

```bash
docker build -t coderushoj/judging-server:dev .
```

镜像使用多阶段构建与 distroless non-root 运行时，不包含编译器、包管理器或 shell。正式镜像由 CI 生成并由 `croj-platform` Helm values 固定 digest。

## Kubernetes 权限

`deploy/kubernetes-rbac.yaml` 提供 namespace 级 ServiceAccount、Role 和 RoleBinding，只允许 `list` EndpointSlice。部署需使用 `coderushoj-judging-server` ServiceAccount；不需要 Secret、Pod、Node 或集群级读取权限。

```bash
kubectl apply -f deploy/kubernetes-rbac.yaml
kubectl auth can-i list endpointslices.discovery.k8s.io \
  --as=system:serviceaccount:coderushoj:coderushoj-judging-server \
  -n coderushoj
```

应用部署、MySQL/RocketMQ 安装、Secret 生成、镜像固定和 Kind 多节点环境由 [`croj-platform`](https://github.com/CodeRushOJ/croj-platform) 统一管理。

## 故障排查

- `no ready sandbox endpoints`：检查 Service selector、EndpointSlice 的 Ready/Terminating 条件和端口名 `grpc`。
- `DeadlineExceeded` / `Unavailable`：检查 sandbox gRPC health、`SANDBOX_EXECUTE_TIMEOUT` 和 Pod 是否正在终止；消息会进入重试路径。
- `forbidden: endpointslices is forbidden`：确认 Deployment 使用正确 ServiceAccount，并应用 RBAC。
- 集群外无法读取 API：检查 `KUBECONFIG` 指向容器内可见路径，必要时以只读方式挂载 kubeconfig。
- RocketMQ 消费失败：核对 NameServer、topic 和 consumer group；消息体必须符合上面的 v1 JSON 契约。
- 回调持续 `401`：确认 judging-server 与 backend 引用同一个 `JUDGE_RESULT_SERVICE_TOKEN` Secret；该错误会按 RocketMQ 低频退避并最终进入 DLQ，应配置告警，服务不会记录 token。
- 回调 `5xx`：消息会重试并复用已缓存结果；检查 backend 健康状态和 `BACKEND_INTERNAL_URL` 的 `/api` context path。
- MySQL 连接失败：确认只读运行时 Secret 已注入，且数据库结构已由后端 Flyway 迁移完成。
- `immutable test bundle is invalid`：核对对象 size/SHA-256、ZIP 内外 manifest 一致性和安全限制；服务不会输出 hidden 内容。
- MinIO 失败：确认 `OBJECT_STORAGE_*`、只读 bucket policy 和 NetworkPolicy；网络错误会重试，确定性缺失/损坏会发布 `SYSTEM_ERROR`。

## 开发与版本

- 需求与验收使用 GitHub Issues 管理；Kubernetes 发现见 Issue #2，真实 gRPC Execute 见 Issue #4。
- Issue #5 交付版本化 RocketMQ payload、稳定 `resultId` 和后端 authenticated/idempotent result callback；判题进程不再直接写 MySQL。
- Issue #10 交付不可变 ACM hidden bundles；Issue #11 交付 sandbox compile-once batch API，隐藏测试主链路一次提交只编译一次。
- Issue #12 跟进 SPJ/OI，Issue #13 跟进原生 OLE callback 状态；未支持能力不会伪报 Accepted。
- 变更通过 `codex/*` 分支和 Draft PR 集成，不直接提交到 `main`。
- 发布遵循 SemVer，并在 GitHub Release 与平台 `CHANGELOG.md` 中记录跨仓库兼容性。
- 提交前必须通过 `go test -race ./...`、`go vet ./...` 和容器构建。

许可证见 [LICENSE](LICENSE)。
