# OpenAPI Contract Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make live response-header validation exhaustive and keep the OpenAPI source-size maximum exactly synchronized with production.

**Architecture:** Test-only header helpers enumerate the recorded HTTP headers, explicitly skip transport/representation headers, resolve every application header in the documented response, validate every raw value with kin-openapi, and scan it for sensitive markers. Production exposes one integer source-size boundary; the OpenAPI test reads the YAML scalar exactly with `yaml.v3` and compares parsed `int64` values.

**Tech Stack:** Go 1.26, `net/http/httptest`, kin-openapi v0.142.0, `gopkg.in/yaml.v3`, OpenAPI 3.1.

---

### Task 1: Exhaustive live response-header validation

**Files:**
- Modify: `internal/httpapi/openapi_contract_test.go`
- Modify: `api/openapi.yaml`

- [ ] **Step 1: Write the failing malicious-header regression**

Add a test that obtains the documented capabilities `401` response, supplies `Content-Type` plus `WWW-Authenticate: Bearer token=credential-value`, invokes `liveResponseHeaderFindings`, and asserts a sensitive-value finding. Extend the live matrix to call the same helper for the entire recorded header map.

- [ ] **Step 2: Run the focused test to verify RED**

Run `go test -count=1 ./internal/httpapi -run 'TestLiveResponseHeaderValidationRejectsSensitiveValues|TestOpenAPIContractCoversLiveHandlerResponses'`. Expect compilation failure because `liveResponseHeaderFindings` is undefined, then after adding only the enumerator expect failures for undocumented `Cache-Control`.

- [ ] **Step 3: Implement the minimal header validator and OpenAPI declaration**

Implement explicit exclusions for `Content-Type`, `Content-Length`, `Transfer-Encoding`, `Date`, `Trailer`, and `Connection`; case-insensitively resolve every other actual header; validate each value against its schema and append `publicValueFindings`. Add reusable `CacheControl` with `const: no-store` and reference it from every public response.

- [ ] **Step 4: Run focused tests to verify GREEN**

Run the same command and expect PASS.

### Task 2: Single exact source-size boundary

**Files:**
- Modify: `internal/httpapi/capabilities.go`
- Modify: `internal/httpapi/jobs.go`
- Modify: `internal/httpapi/server_test.go`
- Modify: `internal/httpapi/openapi_contract_test.go`

- [ ] **Step 1: Write failing boundary and YAML-scalar tests**

Add a production test proving `maximumV1SourceBytes` is accepted and `maximumV1SourceBytes+1` is rejected. Replace the float comparison with a helper that reads `api/openapi.yaml` into a `yaml.Node`, finds `components.schemas.CapabilityLimits.properties.maxSourceBytes.maximum`, parses the scalar with `strconv.ParseInt`, and compares it with `maximumV1SourceBytes`.

- [ ] **Step 2: Run focused tests to verify RED**

Run `go test -count=1 ./internal/httpapi -run 'TestCapabilitiesRejectSourceSizeOutsideV1Boundary|TestOpenAPIContractPinsSecurityHeadersAndAsynchronousSemantics'`. Expect compilation failure because the production constant and YAML helper are absent.

- [ ] **Step 3: Implement the shared production boundary and exact parser**

Define integer constants for the request encoding expansion, envelope allowance, and `maximumV1SourceBytes`; use them from both `normalizeCapabilities` and `maximumJobRequestBytes`. Implement the exact YAML-node lookup helper in the contract test.

- [ ] **Step 4: Run focused tests to verify GREEN**

Run the same command and expect PASS.

### Task 3: Verification and commit

**Files:**
- Verify all modified files.

- [ ] **Step 1: Format and run focused tests**

Run `gofmt` on modified Go files and the focused OpenAPI/capabilities tests.

- [ ] **Step 2: Run repository gates**

Run `go test -race ./...`, `go vet ./...`, the static binary build, and the Docker image build requested by the repository documentation.

- [ ] **Step 3: Review the diff**

Run `git diff --check`, inspect `git diff 4173d3a..HEAD` plus uncommitted changes, and confirm no unrelated changes or sensitive values escaped outside negative tests.

- [ ] **Step 4: Commit without pushing**

Stage the scoped changes and commit with a focused message. Report the new HEAD and all RED/GREEN/gate evidence.
