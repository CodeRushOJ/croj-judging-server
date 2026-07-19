# Canonical Execution Core RED/GREEN Evidence

All Go commands run in an ephemeral `golang:1.26.3` container because the host does not provide Go. The containers use persistent module and build-cache volumes only; no service container is left running.

## Immutable bundle execution limits

### RED

```text
$ go test ./internal/bundle -run 'TestParseManifestV1|TestParseManifestRejectsInvalidContract|TestManifestsMustHaveEqualNormalizedStructure' -count=1
# github.com/CodeRushOJ/croj-judging-server/internal/bundle [github.com/CodeRushOJ/croj-judging-server/internal/bundle.test]
internal/bundle/manifest_test.go:18:14: manifest.Limits undefined (type Manifest has no field or method Limits)
internal/bundle/manifest_test.go:19:37: manifest.Limits undefined (type Manifest has no field or method Limits)
FAIL github.com/CodeRushOJ/croj-judging-server/internal/bundle [build failed]
```

The test failed for the intended reason: manifest v1 did not model authoritative per-bundle execution limits.

### GREEN

```text
$ go test ./internal/bundle -run 'TestParseManifestV1|TestParseManifestRejectsInvalidContract|TestManifestsMustHaveEqualNormalizedStructure' -count=1
ok github.com/CodeRushOJ/croj-judging-server/internal/bundle
```

## Canonical execution request and durable adapter

The following tests were written before their production symbols existed. Their RED builds failed on missing `ExecuteCanonical`, `OpenMetadata`, `LoadClaimInput`, `ClaimCancelled`, and `Runner` implementations:

```text
$ go test ./internal/service ./internal/bundle ./internal/external ./internal/worker -count=1
internal/service/bundle_batch_pipeline_test.go: ExecuteCanonical undefined
internal/bundle/provider_test.go: OpenMetadata undefined
internal/external/mysql_job_worker_test.go: LoadClaimInput undefined; ClaimCancelled undefined
internal/worker/runner_test.go: Runner undefined
FAIL
```

After the minimum implementations were added:

```text
$ go test ./internal/service ./internal/bundle ./internal/external ./internal/worker -count=1
ok github.com/CodeRushOJ/croj-judging-server/internal/service
ok github.com/CodeRushOJ/croj-judging-server/internal/bundle
ok github.com/CodeRushOJ/croj-judging-server/internal/external
ok github.com/CodeRushOJ/croj-judging-server/internal/worker
```

## DNS target and gRPC round robin

RED was a compile failure for the missing DNS target selector and round-robin behavior. GREEN used a manual resolver and two real TCP gRPC servers. A first stress run exposed a readiness race in the assertion; the test was changed to wait for bounded evidence from both ready endpoints, then passed twenty consecutive runs.

```text
$ go test ./internal/sandbox -run TestClientUsesGRPCRoundRobinAcrossResolvedHeadlessServiceEndpoints -count=20
ok github.com/CodeRushOJ/croj-judging-server/internal/sandbox
```

## Explicit external runtime opt-in and readiness

### RED

```text
$ go test ./internal/app -run 'TestRuntime|TestReadiness' -count=1
internal/app/runtime_test.go: undefined: NewRuntime
internal/app/runtime_test.go: undefined: Config
internal/app/runtime_test.go: undefined: Worker
internal/app/runtime_test.go: undefined: Probe
FAIL github.com/CodeRushOJ/croj-judging-server/internal/app [build failed]

$ go test ./pkg/config -run TestExternalAPIIsDisabled -count=1
pkg/config/config_test.go: loaded.ExternalAPI undefined
FAIL github.com/CodeRushOJ/croj-judging-server/pkg/config [build failed]
```

### GREEN

```text
$ go test ./pkg/config ./internal/app ./cmd -count=1
ok github.com/CodeRushOJ/croj-judging-server/pkg/config
ok github.com/CodeRushOJ/croj-judging-server/internal/app
?  github.com/CodeRushOJ/croj-judging-server/cmd [no test files]
```

## Full package gate

```text
$ go test ./...
ok github.com/CodeRushOJ/croj-judging-server/internal/integration
ok github.com/CodeRushOJ/croj-judging-server/internal/sandbox
ok github.com/CodeRushOJ/croj-judging-server/internal/scheduler
ok github.com/CodeRushOJ/croj-judging-server/internal/service
ok github.com/CodeRushOJ/croj-judging-server/internal/worker
ok github.com/CodeRushOJ/croj-judging-server/pkg/config
```

The disposable MySQL 8.4, race, vet, static-build, and container-build outputs are appended during the final delivery gate. Every temporary service container is removed after its focused test.

## MySQL 8.4 authoritative limits and fencing

The focused worker gate uses the pinned MySQL 8.4.10 image and real InnoDB migrations/queries:

```text
$ go test -count=1 -run '^(TestMySQLWorkerLoadsCanonicalInputOnlyForActiveClaim|TestMySQLWorkerHeartbeatCompletionAndRestartReclaimAreFenced)$' ./internal/external
ok github.com/CodeRushOJ/croj-judging-server/internal/external 29.238s
```

This covers two immutable bundles with different execution limits, cancel-request completion without a result, and rejection of a stale completion after lease reclaim.

The first real-SQL ceiling test correctly rejected tenant and platform over-limit bundles and persisted zero MySQL rows, but exposed a staged-object leak:

```text
$ go test -count=1 -run '^TestExternalBundleSQLRepositoryIntegration/tenant_and_platform_execution_ceilings_reject_before_publication$' ./internal/integration
--- FAIL: TestExternalBundleSQLRepositoryIntegration/tenant_and_platform_execution_ceilings_reject_before_publication
    visible bundle objects=1 want=0
FAIL
```

Root cause: a tenant ceiling is authoritatively checked by the SQL commit after staging, while the service discarded staged bytes only for not-found/idempotency-conflict errors. `ErrInvalidBundle` is also a definitive pre-publication rejection. The fix discards for all modeled definitive commit rejections while retaining staging for an outcome-ambiguous generic commit failure; a table-driven unit test covers invalid, tenant-disabled, not-found, conflict, and ambiguous cases.

```text
$ go test -count=1 -run '^(TestBundleServiceDiscardsStagingAfterDefinitiveCommitRejection|TestBundleServiceNeverPublishesUnownedFinalObject)$' ./internal/external
ok github.com/CodeRushOJ/croj-judging-server/internal/external 0.169s

$ go test -count=1 -run '^TestExternalBundleSQLRepositoryIntegration/tenant_and_platform_execution_ceilings_reject_before_publication$' ./internal/integration
ok github.com/CodeRushOJ/croj-judging-server/internal/integration 4.850s
```

## Continued-case aggregation review regression

An Important review gap had a focused RED: with `stopOnFailure=false`, validation accepted two ordered case events but aggregation returned after the first WA and dropped the later AC.

```text
$ go test ./internal/service -run TestBatchBundlePipelineKeepsOrderedResultsWhenStopOnFailureIsDisabled -count=1
--- FAIL: TestBatchBundlePipelineKeepsOrderedResultsWhenStopOnFailureIsDisabled
    canonical result = {Status:WRONG_ANSWER ... Cases:[{CaseID:case-1 Status:WRONG_ANSWER ...}]}
FAIL

$ go test ./internal/service -run 'TestBatchBundlePipeline(CanonicalRequestUsesBundleLimitsAndStopPolicy|KeepsOrderedResultsWhenStopOnFailureIsDisabled|SendsAllCasesInOneCompileOnceRequest)' -count=1
ok github.com/CodeRushOJ/croj-judging-server/internal/service 0.031s
```

Aggregation now retains every executed ordered case and preserves the first failing verdict as the overall result.
