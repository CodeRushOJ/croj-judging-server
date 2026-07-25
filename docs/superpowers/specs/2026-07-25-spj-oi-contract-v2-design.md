# SPJ/OI immutable judge contract v2

## Goal

Deliver deterministic OI weighted scoring and sandboxed special judges without
weakening manifest v1 or exposing checker source/output through result APIs.
Manifest v1 remains ACM `exact`/`token` only.

## Immutable bundle v2

Manifest v2 keeps `limits` and ordered `cases`, and adds:

- `judgeMode`: `ACM` or `OI`;
- `checker`: `exact`, `token`, or `special`;
- `totalScore`: required only for OI and equal to the checked sum of positive
  case weights;
- `specialJudge`: required only for `special`, containing canonical language,
  a safe source path, source SHA-256, and independently bounded time/memory.

The checker source is a referenced UTF-8 ZIP entry and is covered by the
bundle digest plus the manifest source digest. Internal OJ execution also
requires the immutable problem-version checker language/source to match the
bundle. External async REST execution uses the bundle as the sole immutable
source of truth.

## Special-judge ABI

The checker is compiled once by `SandboxService.ExecuteBatchV1`. Each accepted
contestant case produces one checker invocation whose UTF-8 stdin is exactly
one JSON object:

```json
{
  "schemaVersion": 1,
  "caseId": "case-1",
  "input": "...",
  "expectedOutput": "...",
  "actualOutput": "..."
}
```

The checker must write exactly one bounded JSON object:

```json
{"schemaVersion":1,"accepted":true,"message":"optional bounded text"}
```

Unknown fields, trailing JSON, invalid UTF-8, oversized payloads, checker
compile/run failures, and malformed responses are infrastructure errors.
Checker diagnostics and source are never returned to contestants or external
tenants.

## OI scoring

OI always executes every contestant case except a submission compile failure.
Each accepted case earns its immutable positive weight; every other contestant
verdict earns zero. Special-judge acceptance replaces exact/token acceptance
before scoring. The final score is the integer sum in manifest order.

The final verdict is `ACCEPTED` only at `totalScore`; otherwise it is
`WRONG_ANSWER` (compile failure remains `COMPILE_ERROR`). Internal callbacks
and external async REST results carry nullable `score` and `totalScore`;
both are absent for ACM.

## Security and failure boundaries

- Existing archive file/count/decompression limits remain fail-closed.
- Special-judge source is at most 4 MiB and its SHA-256 is verified.
- Each ABI stdin and response is bounded before crossing a sandbox/API
  boundary.
- Checker code runs only through the existing non-root, cgroup/seccomp,
  capability-dropped, network-disabled sandbox path.
- Hidden input, expected output, actual output, checker source, and checker
  diagnostics are excluded from summaries, callbacks, webhooks, and logs.
- Any bundle/config disagreement yields `SYSTEM_ERROR`; it can never become
  `ACCEPTED`.
