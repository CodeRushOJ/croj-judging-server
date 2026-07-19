package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/getkin/kin-openapi/openapi3"
)

func loadOpenAPIContract(t *testing.T) *openapi3.T {
	t.Helper()
	filename := filepath.Join("..", "..", "api", "openapi.yaml")
	document, err := openapi3.NewLoader().LoadFromFile(filename)
	if err != nil {
		t.Fatalf("load %s: %v", filename, err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate %s: %v", filename, err)
	}
	return document
}

func TestOpenAPIContractLoadsAndValidates(t *testing.T) {
	document := loadOpenAPIContract(t)
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version = %q, want 3.1.0", document.OpenAPI)
	}
}

func TestOpenAPIOperationsMatchLiveHTTPHandlers(t *testing.T) {
	document := loadOpenAPIContract(t)
	want := map[string]map[string][]int{
		"/api/v1/capabilities":              {http.MethodGet: {200, 401, 403, 503}},
		"/api/v1/bundles":                   {http.MethodPost: {200, 201, 400, 401, 403, 409, 413, 429, 503}},
		"/api/v1/bundles/{bundleId}":        {http.MethodGet: {200, 401, 403, 404, 503}},
		"/api/v1/judge-jobs":                {http.MethodGet: {200, 400, 401, 403, 500, 503}, http.MethodPost: {202, 400, 401, 403, 404, 409, 422, 429, 500, 503}},
		"/api/v1/judge-jobs/{jobId}":        {http.MethodGet: {200, 401, 403, 404, 500, 503}},
		"/api/v1/judge-jobs/{jobId}/cancel": {http.MethodPost: {200, 401, 403, 404, 500, 503}},
	}

	got := make(map[string]map[string][]int, document.Paths.Len())
	for _, path := range document.Paths.Keys() {
		operations := make(map[string][]int)
		for method, operation := range document.Paths.Value(path).Operations() {
			statuses := make([]int, 0, operation.Responses.Len())
			for _, status := range operation.Responses.Keys() {
				var code int
				if _, err := fmt.Sscanf(status, "%d", &code); err != nil {
					t.Fatalf("%s %s has non-explicit response %q", method, path, status)
				}
				statuses = append(statuses, code)
			}
			sort.Ints(statuses)
			operations[method] = statuses
		}
		got[path] = operations
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenAPI operations/statuses differ from live handler contract\ngot:  %#v\nwant: %#v", got, want)
	}

	server := newOpenAPIProbeServer(t)
	for path, operations := range want {
		for method := range operations {
			requestPath := strings.ReplaceAll(strings.ReplaceAll(path, "{bundleId}", "aaaaaaaaaaaaaaaaaaaaaaaaaa"), "{jobId}", "ceirceirceirceirceirceirce")
			request := httptest.NewRequest(method, requestPath, nil)
			request.Header.Set("Authorization", "Bearer dummy")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code == http.StatusNotFound || response.Code == http.StatusMethodNotAllowed {
				t.Errorf("documented operation is not routed by the live handler: %s %s returned %d", method, requestPath, response.Code)
			}
		}
	}
}

func TestOpenAPIContractPinsSecurityHeadersAndAsynchronousSemantics(t *testing.T) {
	document := loadOpenAPIContract(t)
	scheme := document.Components.SecuritySchemes["BearerAuth"]
	if scheme == nil || scheme.Value == nil || scheme.Value.Type != "http" || scheme.Value.Scheme != "bearer" {
		t.Fatalf("BearerAuth = %#v", scheme)
	}
	if len(document.Security) != 1 || len(document.Security[0]) != 1 {
		t.Fatalf("global security = %#v, want one BearerAuth requirement", document.Security)
	}

	assertParameter := func(t *testing.T, operation *openapi3.Operation, name string, required bool) {
		t.Helper()
		for _, reference := range operation.Parameters {
			if reference.Value != nil && reference.Value.Name == name {
				if reference.Value.Required != required {
					t.Fatalf("parameter %s required=%v, want %v", name, reference.Value.Required, required)
				}
				return
			}
		}
		t.Fatalf("missing parameter %s", name)
	}
	assertParameter(t, operation(t, document, "/api/v1/bundles", http.MethodPost), "Idempotency-Key", true)
	assertParameter(t, operation(t, document, "/api/v1/judge-jobs", http.MethodPost), "Idempotency-Key", true)

	for path, method := range map[string]string{
		"/api/v1/capabilities":              http.MethodGet,
		"/api/v1/bundles":                   http.MethodPost,
		"/api/v1/bundles/{bundleId}":        http.MethodGet,
		"/api/v1/judge-jobs":                http.MethodGet,
		"/api/v1/judge-jobs/{jobId}":        http.MethodGet,
		"/api/v1/judge-jobs/{jobId}/cancel": http.MethodPost,
	} {
		for _, responseRef := range operation(t, document, path, method).Responses.Map() {
			if responseRef.Value == nil || responseRef.Value.Headers["X-Request-Id"] == nil {
				t.Errorf("%s %s response misses X-Request-Id", method, path)
			}
		}
	}

	for _, status := range []int{200, 201} {
		response := operation(t, document, "/api/v1/bundles", http.MethodPost).Responses.Status(status)
		if response.Value.Headers["Location"] == nil {
			t.Errorf("bundle upload %d response misses Location", status)
		}
	}
	jobSubmit := operation(t, document, "/api/v1/judge-jobs", http.MethodPost)
	if jobSubmit.Responses.Status(202).Value.Headers["Location"] == nil || jobSubmit.Responses.Status(202).Value.Headers["Idempotent-Replay"] == nil {
		t.Fatal("job submit 202 must document Location and Idempotent-Replay")
	}
	for _, status := range []int{429, 503} {
		if jobSubmit.Responses.Status(status).Value.Headers["Retry-After"] == nil {
			t.Errorf("job submit %d response misses Retry-After", status)
		}
	}
	if !strings.Contains(document.Info.Description, "v1\\n<event-id-byte-length>\\n<event-id>\\n<timestamp>\\n<raw-body>") ||
		!strings.Contains(document.Info.Description, "X-CodeRushOJ-Event-Id") ||
		!strings.Contains(document.Info.Description, "at-least-once") {
		t.Fatalf("info description does not pin webhook framing and delivery semantics: %q", document.Info.Description)
	}
}

func TestOpenAPIPublicSchemasAreClosedAndDoNotExposeSensitiveFields(t *testing.T) {
	document := loadOpenAPIContract(t)
	for name, reference := range document.Components.Schemas {
		assertClosedObjects(t, "components.schemas."+name, reference, map[*openapi3.Schema]bool{})
	}

	for _, path := range document.Paths.Keys() {
		for method, operation := range document.Paths.Value(path).Operations() {
			for status, responseRef := range operation.Responses.Map() {
				if responseRef.Value == nil {
					continue
				}
				for contentType, media := range responseRef.Value.Content {
					if media.Schema != nil {
						assertNoSensitiveProperties(t, method+" "+path+" "+status+" "+contentType, media.Schema, map[*openapi3.Schema]bool{})
					}
					encoded, err := json.Marshal(media.Example)
					if err != nil {
						t.Fatal(err)
					}
					lower := strings.ToLower(string(encoded))
					for _, forbidden := range []string{"sourcecode", "source_code", "objectkey", "object_key", "stagingkey", "staging_key", "leasetoken", "lease_token", "secret", "expectedoutput", "expected_output", "hiddeninput", "hidden_input", "hiddenoutput", "hidden_output"} {
						if strings.Contains(lower, forbidden) {
							t.Errorf("%s %s %s example contains forbidden token %q", method, path, status, forbidden)
						}
					}
				}
			}
		}
	}
}

func TestOpenAPIJSONExamplesDecodeIntoRealDTOs(t *testing.T) {
	document := loadOpenAPIContract(t)
	tests := []struct {
		name    string
		example any
		target  any
	}{
		{"capabilities response", responseExample(t, document, "/api/v1/capabilities", http.MethodGet, 200), &Capabilities{}},
		{"bundle response", responseExample(t, document, "/api/v1/bundles", http.MethodPost, 201), &external.BundleMetadata{}},
		{"job submit request", requestExample(t, document, "/api/v1/judge-jobs", http.MethodPost), &SubmitJobCommand{}},
		{"job response", responseExample(t, document, "/api/v1/judge-jobs", http.MethodPost, 202), &JobView{}},
		{"job list response", responseExample(t, document, "/api/v1/judge-jobs", http.MethodGet, 200), &JobListPage{}},
		{"problem response", responseExample(t, document, "/api/v1/judge-jobs", http.MethodPost, 400), &Problem{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.example)
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(test.target); err != nil {
				t.Fatalf("decode %s into %T: %v\n%s", test.name, test.target, err, encoded)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				t.Fatalf("%s contains trailing JSON: %v", test.name, err)
			}
		})
	}
}

func TestOpenAPIDocumentationAdvertisesDraftContract(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"api/openapi.yaml", "Draft", "不会启动 HTTP listener"} {
		if !strings.Contains(string(readme), required) {
			t.Errorf("README must contain %q", required)
		}
	}
	changelog, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changelog), "OpenAPI 3.1") || !strings.Contains(string(changelog), "kin-openapi") {
		t.Error("CHANGELOG must record the validated OpenAPI 3.1 contract")
	}
}

func operation(t *testing.T, document *openapi3.T, path, method string) *openapi3.Operation {
	t.Helper()
	pathItem := document.Paths.Value(path)
	if pathItem == nil || pathItem.GetOperation(method) == nil {
		t.Fatalf("missing operation %s %s", method, path)
	}
	return pathItem.GetOperation(method)
}

func requestExample(t *testing.T, document *openapi3.T, path, method string) any {
	t.Helper()
	requestBody := operation(t, document, path, method).RequestBody
	if requestBody == nil || requestBody.Value == nil || requestBody.Value.Content["application/json"] == nil {
		t.Fatalf("missing application/json request body for %s %s", method, path)
	}
	example := requestBody.Value.Content["application/json"].Example
	if example == nil {
		t.Fatalf("missing application/json request example for %s %s", method, path)
	}
	return example
}

func responseExample(t *testing.T, document *openapi3.T, path, method string, status int) any {
	t.Helper()
	response := operation(t, document, path, method).Responses.Status(status)
	if response == nil || response.Value == nil {
		t.Fatalf("missing response %d for %s %s", status, method, path)
	}
	contentType := "application/json"
	if status >= 400 {
		contentType = "application/problem+json"
	}
	media := response.Value.Content[contentType]
	if media == nil || media.Example == nil {
		t.Fatalf("missing %s response example for %d %s %s", contentType, status, method, path)
	}
	return media.Example
}

func assertClosedObjects(t *testing.T, location string, reference *openapi3.SchemaRef, visited map[*openapi3.Schema]bool) {
	t.Helper()
	if reference == nil || reference.Value == nil || visited[reference.Value] {
		return
	}
	schema := reference.Value
	visited[schema] = true
	if schema.Type != nil && schema.Type.Includes("object") && (schema.AdditionalProperties.Has == nil || *schema.AdditionalProperties.Has) {
		t.Errorf("%s object schema must set additionalProperties: false", location)
	}
	for name, property := range schema.Properties {
		assertClosedObjects(t, location+"."+name, property, visited)
	}
	assertClosedObjects(t, location+"[]", schema.Items, visited)
	for index, child := range schema.OneOf {
		assertClosedObjects(t, fmt.Sprintf("%s.oneOf[%d]", location, index), child, visited)
	}
}

func assertNoSensitiveProperties(t *testing.T, location string, reference *openapi3.SchemaRef, visited map[*openapi3.Schema]bool) {
	t.Helper()
	if reference == nil || reference.Value == nil || visited[reference.Value] {
		return
	}
	schema := reference.Value
	visited[schema] = true
	for name, property := range schema.Properties {
		normalized := strings.ToLower(strings.ReplaceAll(name, "_", ""))
		for _, forbidden := range []string{"source", "input", "output", "expected", "objectkey", "stagingkey", "lease", "token", "secret"} {
			if normalized == forbidden || strings.HasPrefix(normalized, forbidden) || strings.HasSuffix(normalized, forbidden) {
				t.Errorf("%s response schema exposes forbidden property %q", location, name)
			}
		}
		assertNoSensitiveProperties(t, location+"."+name, property, visited)
	}
	assertNoSensitiveProperties(t, location+"[]", schema.Items, visited)
	for _, child := range schema.OneOf {
		assertNoSensitiveProperties(t, location, child, visited)
	}
}

func newOpenAPIProbeServer(t *testing.T) *Server {
	t.Helper()
	scopes := map[Scope]struct{}{
		ScopeCapabilitiesRead: {}, ScopeBundleWrite: {}, ScopeBundleRead: {},
		ScopeJobSubmit: {}, ScopeJobRead: {}, ScopeJobCancel: {},
	}
	capabilities := testCapabilities()
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server, err := NewServer(
		staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: scopes}},
		capabilities,
		WithBundleApplication(&bundleApplicationStub{metadata: testBundleMetadata()}),
		WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}),
		WithJobService(&jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}, listPage: JobListPage{Items: []JobView{}}}),
		WithJobWriteQuota(quota, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return server
}
