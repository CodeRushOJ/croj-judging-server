package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticAuthenticator struct {
	principal Principal
	err       error
}

func (auth staticAuthenticator) Authenticate(context.Context, string) (Principal, error) {
	return auth.principal, auth.err
}

func TestCapabilitiesRequiresAuthenticationAndReturnsRFC9457Problem(t *testing.T) {
	server, err := NewServer(staticAuthenticator{err: ErrUnauthenticated}, testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/problem+json" || response.Header().Get("X-Request-Id") == "" {
		t.Fatalf("headers = %#v", response.Header())
	}
	if response.Header().Get("WWW-Authenticate") != `Bearer realm="coderushoj-judge"` {
		t.Fatalf("WWW-Authenticate = %q", response.Header().Get("WWW-Authenticate"))
	}
	var problem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "https://coderushoj.dev/problems/unauthorized" || problem.Status != 401 || problem.RequestID != response.Header().Get("X-Request-Id") {
		t.Fatalf("problem = %+v", problem)
	}
	if strings.Contains(response.Body.String(), "Bearer") {
		t.Fatalf("problem leaked credential material: %s", response.Body.String())
	}
}

func TestCapabilitiesRequiresScope(t *testing.T) {
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeJobRead: {}}}}, testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer secret-never-returned")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestCapabilitiesReturnsRetryableServiceUnavailableWhenAuthenticationStorageFails(t *testing.T) {
	server, err := NewServer(staticAuthenticator{err: fmt.Errorf("%w: mysql 10.0.0.8 timed out", ErrAuthenticationUnavailable)}, testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer syntactically-valid-key")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if strings.Contains(response.Body.String(), "10.0.0.8") || strings.Contains(response.Body.String(), "mysql") {
		t.Fatalf("problem leaked repository details: %s", response.Body.String())
	}
}

func TestCapabilitiesReturnsStableAuthenticatedContract(t *testing.T) {
	server, err := NewServer(staticAuthenticator{principal: Principal{
		TenantID: "tenant-7",
		scopes:   map[Scope]struct{}{ScopeCapabilitiesRead: {}},
	}}, testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer valid-secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var capabilities Capabilities
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.APIVersion != "v1" || len(capabilities.Languages) != 1 || capabilities.Languages[0].ID != "cpp20" || capabilities.Limits.MaxCaseCount != 256 || capabilities.Limits.MaxTimeLimitMillis != 10_000 || capabilities.Limits.MaxMemoryLimitMiB != 1024 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if strings.Contains(response.Body.String(), "tenant-7") || strings.Contains(response.Body.String(), "valid-secret") {
		t.Fatalf("capabilities leaked identity or key: %s", response.Body.String())
	}
}

func TestCapabilitiesRejectsUnsupportedMethods(t *testing.T) {
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeCapabilitiesRead: {}}}}, testCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status=%d allow=%q body=%s", response.Code, response.Header().Get("Allow"), response.Body.String())
	}
}

func testCapabilities() Capabilities {
	return Capabilities{
		APIVersion: "v1",
		Languages:  []LanguageCapability{{ID: "cpp20", DisplayName: "C++ 20", Runtime: "gcc-15"}},
		JudgeModes: []string{"ACM"},
		Checkers:   []string{"EXACT", "TOKEN"},
		Limits: CapabilityLimits{
			MaxSourceBytes:     1 << 20,
			MaxBundleBytes:     64 << 20,
			MaxCaseBytes:       64 << 20,
			MaxCaseCount:       256,
			MaxTimeLimitMillis: 10_000,
			MaxMemoryLimitMiB:  1024,
		},
	}
}
