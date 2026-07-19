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
	"regexp"
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

func TestOpenAPIContractCoversLiveHandlerResponses(t *testing.T) {
	document := loadOpenAPIContract(t)
	type liveCase struct {
		path, method string
		build        func(*testing.T) (*Server, *http.Request)
		status       int
		headers      []string
	}
	jobRequest := func(t *testing.T, service *jobServiceStub) (*Server, *http.Request) {
		server := newJobTestServer(t, service, ScopeJobSubmit)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
		request.Header.Set("Authorization", "Bearer dummy")
		request.Header.Set("Idempotency-Key", "submission-00000042")
		return server, request
	}
	bundleRequest := func(t *testing.T, application *bundleApplicationStub, quota external.Quota) (*Server, *http.Request) {
		if quota == nil {
			quota = &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
		}
		server := newBundleQuotaTestServer(t, application, quota)
		body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer dummy")
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("Idempotency-Key", "upload-key-00001")
		return server, request
	}

	cases := map[string]liveCase{
		"capabilities success": {"/api/v1/capabilities", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeCapabilitiesRead: {}}}}, testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		}, 200, []string{"X-Request-Id"}},
		"missing bearer": {"/api/v1/capabilities", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			authenticator, err := NewAuthenticator(emptyCredentialStore{}, bytes.Repeat([]byte{0x41}, 32))
			if err != nil {
				t.Fatal(err)
			}
			server, err := NewServer(authenticator, testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		}, 401, []string{"X-Request-Id", "WWW-Authenticate"}},
		"malformed bearer": {"/api/v1/capabilities", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			authenticator, err := NewAuthenticator(emptyCredentialStore{}, bytes.Repeat([]byte{0x41}, 32))
			if err != nil {
				t.Fatal(err)
			}
			server, err := NewServer(authenticator, testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
			request.Header.Set("Authorization", "Bearer dummy-not-a-real-key")
			return server, request
		}, 401, []string{"X-Request-Id", "WWW-Authenticate"}},
		"insufficient scope": {"/api/v1/capabilities", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{}}}, testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		}, 403, []string{"X-Request-Id"}},
		"authentication unavailable": {"/api/v1/capabilities", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server, err := NewServer(staticAuthenticator{err: ErrAuthenticationUnavailable}, testCapabilities())
			if err != nil {
				t.Fatal(err)
			}
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"bundle created": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return bundleRequest(t, &bundleApplicationStub{metadata: testBundleMetadata()}, nil)
		}, 201, []string{"X-Request-Id", "Location"}},
		"bundle replay": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return bundleRequest(t, &bundleApplicationStub{metadata: testBundleMetadata(), replay: true}, nil)
		}, 200, []string{"X-Request-Id", "Location"}},
		"bundle conflict": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return bundleRequest(t, &bundleApplicationStub{err: external.ErrIdempotencyConflict}, nil)
		}, 409, []string{"X-Request-Id"}},
		"bundle too large": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return bundleRequest(t, &bundleApplicationStub{err: external.ErrBundleTooLarge}, nil)
		}, 413, []string{"X-Request-Id"}},
		"bundle quota": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: false, RetryAfter: time.Second}}
			return bundleRequest(t, &bundleApplicationStub{}, quota)
		}, 429, []string{"X-Request-Id", "Retry-After"}},
		"bundle publishing": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return bundleRequest(t, &bundleApplicationStub{err: external.ErrBundlePublishing}, nil)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"job accepted": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}})
		}, 202, []string{"X-Request-Id", "Location"}},
		"job replay": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}, replayed: true})
		}, 202, []string{"X-Request-Id", "Location", "Idempotent-Replay"}},
		"job not found": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{err: ErrJobNotFound})
		}, 404, []string{"X-Request-Id"}},
		"job conflict": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{err: ErrIdempotencyConflict})
		}, 409, []string{"X-Request-Id"}},
		"job invalid": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{err: ErrJobInvalid})
		}, 422, []string{"X-Request-Id"}},
		"job internal": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{err: fmt.Errorf("database detail")})
		}, 500, []string{"X-Request-Id"}},
		"job unavailable": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{err: ErrJobUnavailable})
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"invalid list": {"/api/v1/judge-jobs", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{}, ScopeJobRead)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs?limit=0", nil)
		}, 400, []string{"X-Request-Id"}},
		"job get not found": {"/api/v1/judge-jobs/{jobId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{err: ErrJobNotFound}, ScopeJobRead)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
		}, 404, []string{"X-Request-Id"}},
		"job cancel success": {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobCancelled}}, ScopeJobCancel)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel", nil)
		}, 200, []string{"X-Request-Id"}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			server, request := test.build(t)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("live status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
			documented := operation(t, document, test.path, test.method).Responses.Status(response.Code)
			if documented == nil || documented.Value == nil {
				t.Fatalf("live response %d is not documented", response.Code)
			}
			for _, header := range test.headers {
				if response.Header().Get(header) == "" {
					t.Errorf("live response misses required %s", header)
				}
				if documented.Value.Headers[header] == nil {
					t.Errorf("OpenAPI response misses live header %s", header)
				}
			}
			if contentType := strings.Split(response.Header().Get("Content-Type"), ";")[0]; contentType != "" && documented.Value.Content[contentType] == nil {
				t.Errorf("OpenAPI response misses live content type %s", contentType)
			}
		})
	}
}

type emptyCredentialStore struct{}

func (emptyCredentialStore) FindCredentialByPrefix(context.Context, string) (*Credential, error) {
	return nil, nil
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
		!strings.Contains(document.Info.Description, "at-least-once") ||
		!strings.Contains(document.Info.Description, "HMAC-SHA256") ||
		!strings.Contains(document.Info.Description, "v1=<lowercase-hex") ||
		!strings.Contains(document.Info.Description, "Unix seconds") ||
		!strings.Contains(document.Info.Description, "UTF-8 byte length") ||
		!strings.Contains(document.Info.Description, "exact raw body bytes") {
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
					assertSafePublicExample(t, method+" "+path+" "+status+" "+contentType+".example", media.Example)
					for name, exampleRef := range media.Examples {
						if exampleRef != nil && exampleRef.Value != nil {
							assertSafePublicExample(t, method+" "+path+" "+status+" "+contentType+".examples."+name, exampleRef.Value.Value)
						}
					}
				}
			}
		}
	}
}

func TestOpenAPISourceCodeExistsOnlyInSubmitRequest(t *testing.T) {
	document := loadOpenAPIContract(t)
	var locations []string
	for name, reference := range document.Components.Schemas {
		collectPropertyLocations("#/components/schemas/"+name, "sourceCode", reference, map[*openapi3.Schema]bool{}, &locations)
	}
	want := []string{"#/components/schemas/SubmitJobRequest.sourceCode"}
	if !reflect.DeepEqual(locations, want) {
		t.Fatalf("sourceCode property locations = %v, want %v", locations, want)
	}
}

func TestSensitiveExampleValueMarkerRejectsPayloadsButAllowsCapabilityNames(t *testing.T) {
	for name, value := range map[string]string{
		"source":  "sourceCode=int main(){}",
		"hidden":  "hidden input: top secret case",
		"object":  "object_key=tenant/private.zip",
		"storage": "storageUrl=https://private.invalid/bundle",
		"staging": "staging-key: pending/123",
		"lease":   "leaseToken=worker-claim",
		"token":   "token=credential-value",
		"secret":  "secret: credential-value",
	} {
		if marker := sensitiveExampleValueMarker(value); marker == "" {
			t.Errorf("%s leak was not detected in %q", name, value)
		}
	}
	for _, value := range []string{"exact", "token", "Provide a valid active API key.", "The request could not be completed."} {
		if marker := sensitiveExampleValueMarker(value); marker != "" {
			t.Errorf("safe value %q matched %q", value, marker)
		}
	}
}

func TestOpenAPIProblemExamplesMatchHandlerProblemTypes(t *testing.T) {
	document := loadOpenAPIContract(t)
	for name, test := range map[string]struct {
		path, method string
		status       int
		problemType  string
	}{
		"capabilities unavailable": {"/api/v1/capabilities", http.MethodGet, 503, "authentication-unavailable"},
		"invalid bundle":           {"/api/v1/bundles", http.MethodPost, 400, "invalid-bundle"},
		"bundle unavailable":       {"/api/v1/bundles", http.MethodPost, 503, "bundle-unavailable"},
		"bundle not found":         {"/api/v1/bundles/{bundleId}", http.MethodGet, 404, "not-found"},
		"invalid job JSON":         {"/api/v1/judge-jobs", http.MethodPost, 400, "invalid-json"},
		"job not found":            {"/api/v1/judge-jobs", http.MethodPost, 404, "job-not-found"},
		"invalid list query":       {"/api/v1/judge-jobs", http.MethodGet, 400, "invalid-list-query"},
		"job get not found":        {"/api/v1/judge-jobs/{jobId}", http.MethodGet, 404, "job-not-found"},
		"job cancel not found":     {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, 404, "job-not-found"},
	} {
		t.Run(name, func(t *testing.T) {
			want := "https://coderushoj.dev/problems/" + test.problemType
			var got []string
			for _, example := range responseExamples(t, document, test.path, test.method, test.status) {
				object, ok := example.(map[string]any)
				if !ok {
					t.Fatalf("problem example = %T, want object", example)
				}
				got = append(got, fmt.Sprint(object["type"]))
			}
			if !slicesContains(got, want) {
				t.Fatalf("problem types = %v, want %s", got, want)
			}
		})
	}
}

func TestOpenAPIConflictExamplesMatchLiveHandlerDetails(t *testing.T) {
	document := loadOpenAPIContract(t)
	for name, test := range map[string]struct {
		path, detail string
	}{
		"bundle": {"/api/v1/bundles", "The Idempotency-Key was already used for different content."},
		"job":    {"/api/v1/judge-jobs", "The Idempotency-Key is already bound to a different request."},
	} {
		t.Run(name, func(t *testing.T) {
			example := responseExample(t, document, test.path, http.MethodPost, 409)
			object, ok := example.(map[string]any)
			if !ok || object["detail"] != test.detail {
				t.Fatalf("conflict example = %#v, want detail %q", example, test.detail)
			}
		})
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

func responseExamples(t *testing.T, document *openapi3.T, path, method string, status int) []any {
	t.Helper()
	response := operation(t, document, path, method).Responses.Status(status)
	if response == nil || response.Value == nil {
		t.Fatalf("missing response %d for %s %s", status, method, path)
	}
	media := response.Value.Content["application/problem+json"]
	if media == nil {
		t.Fatalf("missing problem response %d for %s %s", status, method, path)
	}
	var examples []any
	if media.Example != nil {
		examples = append(examples, media.Example)
	}
	for _, name := range sortedExampleNames(media.Examples) {
		if reference := media.Examples[name]; reference != nil && reference.Value != nil {
			examples = append(examples, reference.Value.Value)
		}
	}
	if len(examples) == 0 {
		t.Fatalf("missing problem example %d for %s %s", status, method, path)
	}
	return examples
}

func sortedExampleNames(examples openapi3.Examples) []string {
	names := make([]string, 0, len(examples))
	for name := range examples {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
		if identifierExposesSensitiveData(name) {
			t.Errorf("%s response schema exposes forbidden property %q", location, name)
		}
		assertNoSensitiveProperties(t, location+"."+name, property, visited)
	}
	for index, example := range schema.Examples {
		assertSafePublicExample(t, fmt.Sprintf("%s.examples[%d]", location, index), example)
	}
	assertNoSensitiveProperties(t, location+"[]", schema.Items, visited)
	for _, child := range schema.OneOf {
		assertNoSensitiveProperties(t, location, child, visited)
	}
}

var (
	camelWordBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	nonWordCharacter  = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

func identifierWords(identifier string) []string {
	separated := camelWordBoundary.ReplaceAllString(identifier, `${1} ${2}`)
	return strings.Fields(strings.ToLower(nonWordCharacter.ReplaceAllString(separated, " ")))
}

func forbiddenPublicWord(word string) bool {
	switch word {
	case "source", "input", "output", "expected", "object", "storage", "staging", "lease", "token", "secret", "credential", "pod":
		return true
	default:
		return false
	}
}

func identifierExposesSensitiveData(identifier string) bool {
	words := identifierWords(identifier)
	if reflect.DeepEqual(words, []string{"max", "source", "bytes"}) {
		return false
	}
	for _, word := range words {
		if forbiddenPublicWord(word) {
			return true
		}
	}
	return false
}

func assertSafePublicExample(t *testing.T, location string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if identifierExposesSensitiveData(key) {
				t.Errorf("%s exposes forbidden example field %q", location, key)
			}
			assertSafePublicExample(t, location+"."+key, child)
		}
	case []any:
		for index, child := range typed {
			assertSafePublicExample(t, fmt.Sprintf("%s[%d]", location, index), child)
		}
	case string:
		if marker := sensitiveExampleValueMarker(typed); marker != "" {
			t.Errorf("%s contains forbidden example value marker %q", location, marker)
		}
	}
}

func sensitiveExampleValueMarker(value string) string {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"sourcecode", "source_code", "source code",
		"hiddeninput", "hidden_input", "hidden input",
		"hiddenoutput", "hidden_output", "hidden output",
		"expectedoutput", "expected_output", "expected output",
		"objectkey", "object_key", "object-key", "object key",
		"objecturl", "object_url", "object-url", "object url",
		"storagekey", "storage_key", "storage-key", "storage key",
		"storageurl", "storage_url", "storage-url", "storage url",
		"stagingkey", "staging_key", "staging-key", "staging key",
		"stagingurl", "staging_url", "staging-url", "staging url",
		"leasetoken", "lease_token", "lease-token", "lease token",
		"token=", "token:", "secret=", "secret:",
	} {
		if strings.Contains(lower, marker) {
			return marker
		}
	}
	return ""
}

func collectPropertyLocations(location, propertyName string, reference *openapi3.SchemaRef, visited map[*openapi3.Schema]bool, found *[]string) {
	if reference == nil || reference.Value == nil || visited[reference.Value] {
		return
	}
	schema := reference.Value
	visited[schema] = true
	for name, property := range schema.Properties {
		if name == propertyName {
			*found = append(*found, location+"."+name)
		}
		collectPropertyLocations(location+"."+name, propertyName, property, visited, found)
	}
	collectPropertyLocations(location+"[]", propertyName, schema.Items, visited, found)
	for _, child := range schema.OneOf {
		collectPropertyLocations(location, propertyName, child, visited, found)
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
