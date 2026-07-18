# Immutable Hidden Test Bundles Design

## Goal and scope

Make ACM judging correct against immutable hidden cases without changing backend migrations. A submission's non-null `problem_version_id` selects exactly one `t_test_bundle`. Special judge and OI scoring remain unsupported and return `SYSTEM_ERROR`; Issues #11 and #12 track compile-once and SPJ/OI work.

## Authority and artifact format

The object stored in S3/MinIO is a deterministic ZIP whose SHA-256 covers every byte, including `manifest.json` and case files. `manifest.json` is mandatory. The manifest embedded in the ZIP and `t_test_bundle.manifest_json` must both parse through the same strict v1 decoder and produce equal normalized structures. Neither copy overrides the other; disagreement invalidates the bundle.

Manifest v1 fields are `schemaVersion: 1`, `judgeMode: "ACM"`, `checker: "exact" | "token"`, and a non-empty ordered `cases` array. Each case has a unique safe `id`, safe relative `input` and `output` paths, and `weight: 1`. Unknown fields, duplicate IDs or paths, invalid UTF-8, absolute/backslash/traversal paths, zero or out-of-range weights, SPJ, and OI are rejected.

Exact checker uses the current sandbox compatibility rule: normalize CRLF/CR to LF, apply Unicode `TrimSpace` to every line, then trim the joined value. Token checker compares judging-side stdout and expected output as Unicode-whitespace-separated token sequences. Hidden content never appears in judging logs or callback summaries. Rollout is blocked on `croj-sandbox#10`, which removes the legacy sandbox WA expected/actual log.

## Storage and cache

The database remains read-only. `GetTestBundleByProblemVersionID` loads the unique metadata row. S3-compatible storage uses MinIO Go client configuration for endpoint, bucket, region, TLS, access key, and secret key. Downloads stream to a cache-directory temporary file through a byte limit and SHA-256 hasher; metadata size, actual size, configured compressed maximum, and checksum must all agree before atomic rename.

Cache keys are lowercase SHA-256 values. A process-local keyed flight coalesces concurrent misses. The cache validates every hit's size and checksum, deletes corrupt entries, and re-downloads. Completed entries are bounded by byte capacity and TTL; access updates LRU ordering, eviction removes only finalized files. Temporary files are always cleaned after failure.

## ZIP safety

The loader first scans the central directory without extracting. It accepts only regular files, rejects symlinks and all other modes, duplicate normalized names, encrypted/unsupported records, suspicious compression ratios, per-file or total uncompressed size overflow, excessive file count, and any path traversal. Reads use each entry's declared size plus a hard limiting reader and verify exact byte counts. Case input/output and manifest sizes have independent limits. No filesystem extraction occurs, eliminating symlink races.

## Execution and errors

The runner executes cases in manifest order. Each request contains immutable source, case stdin, expected output for exact checking, version limits, and one selected Ready endpoint. `Accepted` continues; CE/WA/TLE/MLE/RE/OLE stop immediately. Exact repeats the sandbox comparison in judging so an empty expected output cannot bypass comparison. Token runs with empty expected output, then judging compares stdout tokens. OLE temporarily maps to callback v1 `RUNTIME_ERROR` and retains only the OLE label in its summary. Time and memory aggregate by maximum, and callback text contains only bounded case IDs/verdict summaries.

gRPC `Unavailable`/`ResourceExhausted`, `Sandbox Error`, and unknown sandbox statuses are infrastructure failures. The runner selects another Ready endpoint up to a configured attempt limit for the same case, then returns `SYSTEM_ERROR`. Contestant verdicts are never retried. Missing metadata, null problem version, invalid bundle, unsupported manifest, or corrupt storage return a publishable `SYSTEM_ERROR` result rather than a retry loop; transient database/object-store/network failures remain retryable errors.

## Verification

Tests cover strict manifest equality, traversal/symlink/zip-bomb rejection, download size/checksum, cache concurrency/corruption/TTL/LRU, exact/token comparison, multi-case ordering/early stop/aggregation, bounded endpoint failover, and missing/bad bundles. Required gates are race tests, vet, static build, and non-root container build. Services are not started.
