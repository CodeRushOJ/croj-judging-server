# External OJ OpenAPI 3.1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a validated OpenAPI 3.1 contract for the existing asynchronous external OJ REST handlers without exposing source code, hidden tests, storage internals, credentials, or worker state.

**Architecture:** `api/openapi.yaml` is a documentation artifact whose executable contract tests load and validate it with pinned `kin-openapi`. Tests keep the path/method inventory aligned with the actual `httpapi.Server` router, validate headers/status/security, recursively audit public response schemas, and decode JSON examples into the real Go DTOs.

**Tech Stack:** OpenAPI 3.1 YAML, Go 1.26.3, `github.com/getkin/kin-openapi` v0.142.0, Docker-based test/build gates, GitHub Actions.

---

### Task 1: Pin the validator and establish RED contract tests

**Files:**
- Create: `internal/httpapi/openapi_contract_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] Add `github.com/getkin/kin-openapi v0.142.0` as a test dependency.
- [ ] Write a test that loads `../../api/openapi.yaml` with `openapi3.NewLoader()` and calls `Validate`.
- [ ] Assert the exact implemented operation set: `GET /api/v1/capabilities`, `POST /api/v1/bundles`, `GET /api/v1/bundles/{bundleId}`, `POST/GET /api/v1/judge-jobs`, `GET /api/v1/judge-jobs/{jobId}`, and `POST /api/v1/judge-jobs/{jobId}/cancel`.
- [ ] Add assertions for bearer security, required headers, handler-supported response statuses/headers, strict object schemas, response-sensitive-field denial, and real-DTO JSON example decoding.
- [ ] Run the focused test in `golang:1.26.3`; verify it fails because `api/openapi.yaml` does not exist.

### Task 2: Implement the OpenAPI 3.1 document

**Files:**
- Create: `api/openapi.yaml`

- [ ] Define `openapi: 3.1.0`, the private beta server prefix, bearer API-key security, reusable request/response headers, RFC 9457 Problem, and strict reusable schemas.
- [ ] Document capabilities, bundle upload/metadata, job submit/list/get/cancel using only statuses emitted by current handlers.
- [ ] Add copyable curl examples with short, explicit dummy tokens and non-sensitive sample payloads.
- [ ] Describe polling, replay/conflict, tenant-scoped `404`, cancellation semantics, and webhook v1 HMAC framing/headers/at-least-once deduplication without adding a webhook endpoint.
- [ ] Run the focused contract test and iterate until it passes.

### Task 3: Document discoverability and release history

**Files:**
- Modify: `README.md`
- Modify: `CHANGELOG.md`

- [ ] Add a prominent external integration quick-start link to `api/openapi.yaml`, including capabilities → bundle → submit → poll/cancel order.
- [ ] State explicitly that the API remains Draft/beta and no HTTP listener is started by this branch.
- [ ] Add the OpenAPI contract and validator gate to `[Unreleased]`.
- [ ] Run the focused contract tests again.

### Task 4: Verify CI coverage and all repository gates

**Files:**
- Inspect: `.github/workflows/ci.yml`

- [ ] Confirm the existing `go test -race ./...` step necessarily executes `openapi_contract_test.go`; do not weaken Redis or MySQL jobs.
- [ ] Run `go test -race ./...` in `golang:1.26.3`.
- [ ] Run `go vet ./...`.
- [ ] Build `./cmd` and `./cmd/judge-admin` statically with `CGO_ENABLED=0`.
- [ ] Run `git diff --check` and inspect the full diff for accidental secrets or sensitive response examples.

### Task 5: Independent reviews and publication

**Files:**
- Modify as required by review findings.

- [ ] Request a spec-focused review against the exact requirements and fix all Critical/Important findings.
- [ ] Request a separate code-quality/test review and fix all Critical/Important findings.
- [ ] Re-run all gates after review fixes.
- [ ] Commit intentional changes, push `codex/external-openapi`, and open a Draft PR targeting `codex/external-bundle-upload`.
