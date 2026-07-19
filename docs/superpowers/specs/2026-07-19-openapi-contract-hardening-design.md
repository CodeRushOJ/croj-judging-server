# OpenAPI Contract Hardening Design

## Goal

Close two contract-test gaps: validate and scan every application response header value emitted by the live handlers, and keep the OpenAPI `maxSourceBytes` maximum exactly synchronized with the production capabilities boundary without floating-point comparison.

## Live response headers

The live contract matrix will retain its required-header assertions and additionally pass the complete recorded response header map to a test-only validator. The validator will explicitly ignore only transport or representation headers handled outside OpenAPI response `headers`: `Content-Type`, `Content-Length`, `Transfer-Encoding`, `Date`, `Trailer`, and `Connection`.

Every other actual header must be declared by the selected OpenAPI response. Every value must validate against the declared kin-openapi schema and pass the existing sensitive-value scanner. A regression test will inject a malicious `WWW-Authenticate` value containing `token=credential-value` and require a sensitive-data finding.

`Cache-Control` is set intentionally by the handlers, so every documented response will expose a reusable header schema fixed to `no-store`.

## Source-size boundary

Production will define one package-level integer constant for the maximum v1 source size. Both capabilities normalization and the job-request byte-limit calculation will use the same boundary constants. Tests will prove that the exact maximum is accepted and maximum plus one is rejected.

The OpenAPI contract test will read the YAML maximum as an unmodified scalar through `yaml.v3`, parse it as `int64`, and compare it with the production constant. It will not compare kin-openapi's floating-point representation of the JSON Schema number.

## Verification

Each fix follows a separate RED/GREEN cycle. Final verification covers focused OpenAPI/capabilities tests, full race tests, `go vet`, both requested builds, and a scoped diff review. No push is performed.
