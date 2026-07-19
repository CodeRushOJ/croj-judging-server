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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
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
	jobRawRequest := func(t *testing.T, service *jobServiceStub, body string, keys ...string) (*Server, *http.Request) {
		server := newJobTestServer(t, service, ScopeJobSubmit)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer dummy")
		for _, key := range keys {
			request.Header.Add("Idempotency-Key", key)
		}
		return server, request
	}
	jobRequest := func(t *testing.T, service *jobServiceStub) (*Server, *http.Request) {
		return jobRawRequest(t, service, `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`, "submission-00000042")
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
	newBundleServer := func(t *testing.T, authenticator RequestAuthenticator, application BundleApplication, quota external.Quota) *Server {
		if quota == nil {
			quota = &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
		}
		capabilities := testCapabilities()
		server, err := NewServer(authenticator, capabilities, WithBundleApplication(application),
			WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}))
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	newJobServer := func(t *testing.T, authenticator RequestAuthenticator, service JobService, quota external.Quota) *Server {
		if quota == nil {
			quota = &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
		}
		server, err := NewServer(authenticator, testCapabilities(), WithJobService(service),
			WithJobWriteQuota(quota, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}))
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	allScopes := map[Scope]struct{}{
		ScopeCapabilitiesRead: {}, ScopeBundleWrite: {}, ScopeBundleRead: {},
		ScopeJobSubmit: {}, ScopeJobRead: {}, ScopeJobCancel: {},
	}
	validBundleUpload := func(t *testing.T) *http.Request {
		body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("Idempotency-Key", "upload-key-00001")
		return request
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
		"bundle invalid upload": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &bundleApplicationStub{}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/bundles", nil)
		}, 400, []string{"X-Request-Id"}},
		"bundle unauthenticated": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{err: ErrUnauthenticated}, &bundleApplicationStub{}, nil)
			return server, validBundleUpload(t)
		}, 401, []string{"X-Request-Id", "WWW-Authenticate"}},
		"bundle forbidden": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{}}}, &bundleApplicationStub{}, nil)
			return server, validBundleUpload(t)
		}, 403, []string{"X-Request-Id"}},
		"bundle quota unavailable": {"/api/v1/bundles", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			quota := &writeQuotaStub{err: external.ErrQuotaUnavailable}
			return bundleRequest(t, &bundleApplicationStub{}, quota)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"bundle get success": {"/api/v1/bundles/{bundleId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &bundleApplicationStub{metadata: testBundleMetadata()}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/bundles/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
		}, 200, []string{"X-Request-Id"}},
		"bundle get not found": {"/api/v1/bundles/{bundleId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &bundleApplicationStub{err: external.ErrBundleNotFound}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/bundles/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
		}, 404, []string{"X-Request-Id"}},
		"bundle get unavailable": {"/api/v1/bundles/{bundleId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newBundleServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &bundleApplicationStub{err: fmt.Errorf("store unavailable")}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/bundles/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
		}, 503, []string{"X-Request-Id"}},
		"job accepted": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}})
		}, 202, []string{"X-Request-Id", "Location"}},
		"job replay": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRequest(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}, replayed: true})
		}, 202, []string{"X-Request-Id", "Location", "Idempotent-Replay"}},
		"job missing idempotency": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRawRequest(t, &jobServiceStub{}, `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`)
		}, 400, []string{"X-Request-Id"}},
		"job repeated idempotency": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRawRequest(t, &jobServiceStub{}, `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`, "submission-00000042", "submission-00000043")
		}, 400, []string{"X-Request-Id"}},
		"job invalid idempotency": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRawRequest(t, &jobServiceStub{}, `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`, "short")
		}, 400, []string{"X-Request-Id"}},
		"job invalid JSON": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			return jobRawRequest(t, &jobServiceStub{}, `{"sourceCode":`, "submission-00000042")
		}, 400, []string{"X-Request-Id"}},
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
		"job unauthenticated": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{err: ErrUnauthenticated}, &jobServiceStub{}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", nil)
		}, 401, []string{"X-Request-Id", "WWW-Authenticate"}},
		"job forbidden": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{}}}, &jobServiceStub{}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", nil)
		}, 403, []string{"X-Request-Id"}},
		"job quota exceeded": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: false, RetryAfter: time.Second}}
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce"}}, quota)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
			request.Header.Set("Idempotency-Key", "submission-00000042")
			return server, request
		}, 429, []string{"X-Request-Id", "Retry-After"}},
		"job quota unavailable": {"/api/v1/judge-jobs", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			quota := &writeQuotaStub{err: external.ErrQuotaUnavailable}
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce"}}, quota)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
			request.Header.Set("Idempotency-Key", "submission-00000042")
			return server, request
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"invalid list": {"/api/v1/judge-jobs", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{}, ScopeJobRead)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs?limit=0", nil)
		}, 400, []string{"X-Request-Id"}},
		"job list success": {"/api/v1/judge-jobs", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{listPage: JobListPage{Items: []JobView{}}}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs", nil)
		}, 200, []string{"X-Request-Id"}},
		"job list unavailable": {"/api/v1/judge-jobs", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: ErrJobUnavailable}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs", nil)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"job list internal": {"/api/v1/judge-jobs", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: fmt.Errorf("database detail")}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs", nil)
		}, 500, []string{"X-Request-Id"}},
		"job get success": {"/api/v1/judge-jobs/{jobId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobRunning}}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
		}, 200, []string{"X-Request-Id"}},
		"job get not found": {"/api/v1/judge-jobs/{jobId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{err: ErrJobNotFound}, ScopeJobRead)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
		}, 404, []string{"X-Request-Id"}},
		"job get unavailable": {"/api/v1/judge-jobs/{jobId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: ErrJobUnavailable}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"job get internal": {"/api/v1/judge-jobs/{jobId}", http.MethodGet, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: fmt.Errorf("database detail")}, nil)
			return server, httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
		}, 500, []string{"X-Request-Id"}},
		"job cancel success": {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobTestServer(t, &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobCancelled}}, ScopeJobCancel)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel", nil)
		}, 200, []string{"X-Request-Id"}},
		"job cancel not found": {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: ErrJobNotFound}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel", nil)
		}, 404, []string{"X-Request-Id"}},
		"job cancel unavailable": {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: ErrJobUnavailable}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel", nil)
		}, 503, []string{"X-Request-Id", "Retry-After"}},
		"job cancel internal": {"/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, func(t *testing.T) (*Server, *http.Request) {
			server := newJobServer(t, staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: allScopes}}, &jobServiceStub{err: fmt.Errorf("database detail")}, nil)
			return server, httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel", nil)
		}, 500, []string{"X-Request-Id"}},
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
				value := response.Header().Get(header)
				if value == "" {
					t.Errorf("live response misses required %s", header)
				}
			}
			for _, finding := range liveResponseHeaderFindings(response.Header(), documented.Value) {
				t.Error(finding)
			}
			contentType := strings.Split(response.Header().Get("Content-Type"), ";")[0]
			media := documented.Value.Content[contentType]
			if contentType != "" && media == nil {
				t.Fatalf("OpenAPI response misses live content type %s", contentType)
			}
			if response.Body.Len() > 0 {
				if media == nil || media.Schema == nil || media.Schema.Value == nil {
					t.Fatalf("OpenAPI response misses schema for live content type %s", contentType)
				}
				var body any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode live response body: %v", err)
				}
				if err := media.Schema.Value.VisitJSON(body); err != nil {
					t.Errorf("live response body violates OpenAPI schema: %v; body=%s", err, response.Body.String())
				}
				assertSafePublicExample(t, "live response body", body)
			}
			if response.Code >= 400 {
				var actual Problem
				if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
					t.Fatalf("decode live problem: %v", err)
				}
				represented := false
				for _, example := range responseExamples(t, document, test.path, test.method, response.Code) {
					var documentedProblem Problem
					decodeStrictExample(t, "documented live problem", example, &documentedProblem)
					if actual.Type == documentedProblem.Type && actual.Title == documentedProblem.Title && actual.Status == documentedProblem.Status && actual.Detail == documentedProblem.Detail {
						represented = true
						break
					}
				}
				if !represented {
					t.Errorf("live problem is not represented by OpenAPI examples: %+v", actual)
				}
			}
		})
	}
}

func TestLiveResponseHeaderValidationRejectsSensitiveValues(t *testing.T) {
	document := loadOpenAPIContract(t)
	documented := operation(t, document, "/api/v1/capabilities", http.MethodGet).Responses.Status(http.StatusUnauthorized)
	actual := http.Header{
		"Content-Type":     {"application/problem+json"},
		"WWW-Authenticate": {"Bearer token=credential-value"},
	}

	findings := strings.Join(liveResponseHeaderFindings(actual, documented.Value), "\n")
	if !strings.Contains(findings, `contains forbidden example value marker "token="`) {
		t.Fatalf("malicious live header was not rejected:\n%s", findings)
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
	bundleLocation := operation(t, document, "/api/v1/bundles", http.MethodPost).Responses.Status(201).Value.Headers["Location"]
	jobLocation := jobSubmit.Responses.Status(202).Value.Headers["Location"]
	if bundleLocation == nil || bundleLocation.Value == nil || !strings.HasPrefix(fmt.Sprint(bundleLocation.Value.Example), "/api/v1/bundles/") {
		t.Fatalf("bundle Location example = %#v", bundleLocation)
	}
	if jobLocation == nil || jobLocation.Value == nil || !strings.HasPrefix(fmt.Sprint(jobLocation.Value.Example), "/api/v1/judge-jobs/") {
		t.Fatalf("job Location example = %#v", jobLocation)
	}
	for name, test := range map[string]struct {
		header  *openapi3.HeaderRef
		pattern string
	}{
		"bundle": {bundleLocation, `^/api/v1/bundles/[a-z2-7]{26}$`},
		"job":    {jobLocation, `^/api/v1/judge-jobs/[a-z2-7]{26}$`},
	} {
		if test.header.Value.Schema == nil || test.header.Value.Schema.Value == nil || test.header.Value.Schema.Value.Pattern != test.pattern {
			t.Errorf("%s Location pattern = %#v, want %q", name, test.header.Value.Schema, test.pattern)
		}
	}
	maxSource := openAPIIntegerScalar(t, "components", "schemas", "CapabilityLimits", "properties", "maxSourceBytes", "maximum")
	if maxSource != maximumV1SourceBytes {
		t.Errorf("CapabilityLimits.maxSourceBytes maximum = %d, want %d", maxSource, maximumV1SourceBytes)
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
	audit := func(location string, reference *openapi3.SchemaRef, sensitive bool) {
		t.Helper()
		for _, finding := range schemaAuditFindings(location, reference, sensitive, map[*openapi3.Schema]bool{}) {
			t.Error(finding)
		}
	}
	for name, reference := range document.Components.Schemas {
		audit("components.schemas."+name, reference, false)
	}
	for name, reference := range document.Components.Parameters {
		if reference != nil && reference.Value != nil {
			audit("components.parameters."+name, reference.Value.Schema, false)
		}
	}
	for name, reference := range document.Components.Headers {
		if reference != nil && reference.Value != nil {
			audit("components.headers."+name, reference.Value.Schema, true)
		}
	}

	for _, path := range document.Paths.Keys() {
		pathItem := document.Paths.Value(path)
		for index, parameterRef := range pathItem.Parameters {
			if parameterRef != nil && parameterRef.Value != nil {
				audit(fmt.Sprintf("path %s parameters[%d]", path, index), parameterRef.Value.Schema, false)
			}
		}
		for method, operation := range pathItem.Operations() {
			prefix := method + " " + path
			for index, parameterRef := range operation.Parameters {
				if parameterRef != nil && parameterRef.Value != nil {
					audit(fmt.Sprintf("%s parameters[%d]", prefix, index), parameterRef.Value.Schema, false)
				}
			}
			if operation.RequestBody != nil && operation.RequestBody.Value != nil {
				for contentType, media := range operation.RequestBody.Value.Content {
					allowSource := method == http.MethodPost && path == "/api/v1/judge-jobs"
					audit(prefix+" request "+contentType, media.Schema, !allowSource)
					auditPublicValue(t, prefix+" request "+contentType+".example", media.Example, !allowSource)
					for name, exampleRef := range media.Examples {
						if exampleRef != nil && exampleRef.Value != nil {
							auditPublicValue(t, prefix+" request "+contentType+".examples."+name, exampleRef.Value.Value, !allowSource)
						}
					}
				}
			}
			for status, responseRef := range operation.Responses.Map() {
				if responseRef.Value == nil {
					continue
				}
				for name, headerRef := range responseRef.Value.Headers {
					if headerRef != nil && headerRef.Value != nil {
						audit(prefix+" "+status+" header "+name, headerRef.Value.Schema, true)
						auditPublicValue(t, prefix+" "+status+" header "+name+".example", headerRef.Value.Example, true)
						for exampleName, exampleRef := range headerRef.Value.Examples {
							if exampleRef != nil && exampleRef.Value != nil {
								auditPublicValue(t, prefix+" "+status+" header "+name+".examples."+exampleName, exampleRef.Value.Value, true)
							}
						}
					}
				}
				for contentType, media := range responseRef.Value.Content {
					location := prefix + " " + status + " " + contentType
					audit(location, media.Schema, true)
					auditPublicValue(t, location+".example", media.Example, true)
					for name, exampleRef := range media.Examples {
						if exampleRef != nil && exampleRef.Value != nil {
							auditPublicValue(t, location+".examples."+name, exampleRef.Value.Value, true)
						}
					}
				}
			}
		}
	}
}

func TestSchemaAuditTraversesOpenAPI31CompositionAndImplicitObjects(t *testing.T) {
	root := &openapi3.Schema{
		Properties: openapi3.Schemas{"plain": {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}},
		AllOf:      openapi3.SchemaRefs{{Value: &openapi3.Schema{Properties: openapi3.Schemas{"storageKey": {Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}}}}},
		AnyOf:      openapi3.SchemaRefs{{Value: &openapi3.Schema{Example: map[string]any{"secret": "credential-value"}}}},
		OneOf:      openapi3.SchemaRefs{{Value: &openapi3.Schema{Default: map[string]any{"leaseToken": "worker-claim"}}}},
		Not:        &openapi3.SchemaRef{Value: &openapi3.Schema{Enum: []any{map[string]any{"objectKey": "private/tests.zip"}}}},
	}
	findings := strings.Join(schemaAuditFindings("root", &openapi3.SchemaRef{Value: root}, true, map[*openapi3.Schema]bool{}), "\n")
	for _, want := range []string{"root object schema", "allOf", "storageKey", "anyOf", "secret", "oneOf", "leaseToken", "not", "objectKey"} {
		if !strings.Contains(findings, want) {
			t.Errorf("audit findings do not prove traversal of %q:\n%s", want, findings)
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
		{"bundle replay response", responseExample(t, document, "/api/v1/bundles", http.MethodPost, 200), &external.BundleMetadata{}},
		{"bundle created response", responseExample(t, document, "/api/v1/bundles", http.MethodPost, 201), &external.BundleMetadata{}},
		{"bundle metadata response", responseExample(t, document, "/api/v1/bundles/{bundleId}", http.MethodGet, 200), &external.BundleMetadata{}},
		{"job submit request", requestExample(t, document, "/api/v1/judge-jobs", http.MethodPost), &SubmitJobCommand{}},
		{"job submit response", responseExample(t, document, "/api/v1/judge-jobs", http.MethodPost, 202), &JobView{}},
		{"job list response", responseExample(t, document, "/api/v1/judge-jobs", http.MethodGet, 200), &JobListPage{}},
		{"job detail response", responseExample(t, document, "/api/v1/judge-jobs/{jobId}", http.MethodGet, 200), &JobView{}},
		{"job cancel response", responseExample(t, document, "/api/v1/judge-jobs/{jobId}/cancel", http.MethodPost, 200), &JobView{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decodeStrictExample(t, test.name, test.example, test.target)
		})
	}

	for _, path := range document.Paths.Keys() {
		for method, operation := range document.Paths.Value(path).Operations() {
			for statusText := range operation.Responses.Map() {
				var status int
				if _, err := fmt.Sscanf(statusText, "%d", &status); err != nil || status < 400 {
					continue
				}
				for index, example := range responseExamples(t, document, path, method, status) {
					var problem Problem
					decodeStrictExample(t, fmt.Sprintf("%s %s %d problem[%d]", method, path, status, index), example, &problem)
					if problem.Status != status {
						t.Errorf("%s %s response %d example status = %d", method, path, status, problem.Status)
					}
				}
			}
		}
	}
}

func decodeStrictExample(t *testing.T, name string, example, target any) {
	t.Helper()
	encoded, err := json.Marshal(example)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode %s into %T: %v\n%s", name, target, err, encoded)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("%s contains trailing JSON: %v", name, err)
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

func openAPIIntegerScalar(t *testing.T, keys ...string) int64 {
	t.Helper()
	filename := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode %s as YAML nodes: %v", filename, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		t.Fatalf("%s has unexpected YAML document root", filename)
	}
	node := root.Content[0]
	for _, key := range keys {
		if node.Kind != yaml.MappingNode {
			t.Fatalf("OpenAPI YAML path %s reaches non-mapping node", strings.Join(keys, "."))
		}
		var next *yaml.Node
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				next = node.Content[index+1]
				break
			}
		}
		if next == nil {
			t.Fatalf("OpenAPI YAML path %s is missing key %q", strings.Join(keys, "."), key)
		}
		node = next
	}
	if node.Kind != yaml.ScalarNode {
		t.Fatalf("OpenAPI YAML path %s is not a scalar", strings.Join(keys, "."))
	}
	value, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		t.Fatalf("OpenAPI YAML path %s is not an exact int64: %v", strings.Join(keys, "."), err)
	}
	return value
}

func liveResponseHeaderFindings(actual http.Header, documented *openapi3.Response) []string {
	if documented == nil {
		return []string{"OpenAPI response is missing"}
	}
	var findings []string
	names := make([]string, 0, len(actual))
	for name := range actual {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if implicitHTTPResponseHeader(name) {
			continue
		}
		reference := documented.Headers[name]
		if reference == nil {
			for documentedName, candidate := range documented.Headers {
				if strings.EqualFold(documentedName, name) {
					reference = candidate
					break
				}
			}
		}
		if reference == nil {
			findings = append(findings, fmt.Sprintf("OpenAPI response misses live header %s", name))
			continue
		}
		for index, raw := range actual[name] {
			location := fmt.Sprintf("live response header %s[%d]", name, index)
			findings = append(findings, liveHeaderValueFindings(location, raw, reference)...)
			findings = append(findings, publicValueFindings(location, raw)...)
		}
	}
	return findings
}

func implicitHTTPResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Content-Length", "Content-Type", "Date", "Trailer", "Transfer-Encoding":
		return true
	default:
		return false
	}
}

func liveHeaderValueFindings(name, raw string, reference *openapi3.HeaderRef) []string {
	if reference.Value == nil || reference.Value.Schema == nil || reference.Value.Schema.Value == nil {
		return []string{fmt.Sprintf("OpenAPI header %s has no schema", name)}
	}
	schema := reference.Value.Schema.Value
	var value any = raw
	if schema.Type != nil && schema.Type.Includes("integer") {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return []string{fmt.Sprintf("live header %s=%q is not an integer: %v", name, raw, err)}
		}
		value = parsed
	} else if schema.Type != nil && schema.Type.Includes("boolean") {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return []string{fmt.Sprintf("live header %s=%q is not a boolean: %v", name, raw, err)}
		}
		value = parsed
	}
	if err := schema.VisitJSON(value); err != nil {
		return []string{fmt.Sprintf("live header %s=%q violates OpenAPI schema: %v", name, raw, err)}
	}
	return nil
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

func schemaAuditFindings(location string, reference *openapi3.SchemaRef, sensitive bool, visited map[*openapi3.Schema]bool) []string {
	if reference == nil || reference.Value == nil || visited[reference.Value] {
		return nil
	}
	schema := reference.Value
	visited[schema] = true
	var findings []string
	isObject := len(schema.Properties) > 0 || len(schema.PatternProperties) > 0 || (schema.Type != nil && schema.Type.Includes("object"))
	if isObject && (schema.AdditionalProperties.Has == nil || *schema.AdditionalProperties.Has) {
		findings = append(findings, location+" object schema must set additionalProperties: false")
	}
	if sensitive {
		findings = append(findings, publicValueFindings(location+".example", schema.Example)...)
		findings = append(findings, publicValueFindings(location+".default", schema.Default)...)
		findings = append(findings, publicValueFindings(location+".const", schema.Const)...)
		for index, value := range schema.Examples {
			findings = append(findings, publicValueFindings(fmt.Sprintf("%s.examples[%d]", location, index), value)...)
		}
		for index, value := range schema.Enum {
			findings = append(findings, publicValueFindings(fmt.Sprintf("%s.enum[%d]", location, index), value)...)
		}
	}
	for name, property := range schema.Properties {
		childLocation := location + ".properties." + name
		if sensitive && identifierExposesSensitiveData(name) {
			findings = append(findings, fmt.Sprintf("%s exposes forbidden property %q", childLocation, name))
		}
		findings = append(findings, schemaAuditFindings(childLocation, property, sensitive, visited)...)
	}
	for name, property := range schema.PatternProperties {
		findings = append(findings, schemaAuditFindings(location+".patternProperties."+name, property, sensitive, visited)...)
	}
	for name, property := range schema.DependentSchemas {
		findings = append(findings, schemaAuditFindings(location+".dependentSchemas."+name, property, sensitive, visited)...)
	}
	for name, property := range schema.Defs {
		findings = append(findings, schemaAuditFindings(location+".$defs."+name, property, sensitive, visited)...)
	}
	findings = append(findings, schemaAuditFindings(location+".items", schema.Items, sensitive, visited)...)
	findings = append(findings, schemaAuditFindings(location+".contains", schema.Contains, sensitive, visited)...)
	findings = append(findings, schemaAuditFindings(location+".propertyNames", schema.PropertyNames, sensitive, visited)...)
	findings = append(findings, schemaAuditFindings(location+".additionalProperties", schema.AdditionalProperties.Schema, sensitive, visited)...)
	findings = append(findings, schemaAuditFindings(location+".contentSchema", schema.ContentSchema, sensitive, visited)...)
	for index, child := range schema.PrefixItems {
		findings = append(findings, schemaAuditFindings(fmt.Sprintf("%s.prefixItems[%d]", location, index), child, sensitive, visited)...)
	}
	for keyword, children := range map[string]openapi3.SchemaRefs{"allOf": schema.AllOf, "anyOf": schema.AnyOf, "oneOf": schema.OneOf} {
		for index, child := range children {
			findings = append(findings, schemaAuditFindings(fmt.Sprintf("%s.%s[%d]", location, keyword, index), child, sensitive, visited)...)
		}
	}
	for keyword, child := range map[string]*openapi3.SchemaRef{"not": schema.Not, "if": schema.If, "then": schema.Then, "else": schema.Else} {
		findings = append(findings, schemaAuditFindings(location+"."+keyword, child, sensitive, visited)...)
	}
	return findings
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
	for _, finding := range publicValueFindings(location, value) {
		t.Error(finding)
	}
}

func auditPublicValue(t *testing.T, location string, value any, sensitive bool) {
	t.Helper()
	if sensitive {
		assertSafePublicExample(t, location, value)
	}
}

func publicValueFindings(location string, value any) []string {
	var findings []string
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if identifierExposesSensitiveData(key) {
				findings = append(findings, fmt.Sprintf("%s exposes forbidden example field %q", location, key))
			}
			findings = append(findings, publicValueFindings(location+"."+key, child)...)
		}
	case []any:
		for index, child := range typed {
			findings = append(findings, publicValueFindings(fmt.Sprintf("%s[%d]", location, index), child)...)
		}
	case string:
		if marker := sensitiveExampleValueMarker(typed); marker != "" {
			findings = append(findings, fmt.Sprintf("%s contains forbidden example value marker %q", location, marker))
		}
	}
	return findings
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
