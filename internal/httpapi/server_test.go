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
	if capabilities.APIVersion != "v1" || len(capabilities.Languages) != 1 || capabilities.Languages[0].ID != "cpp20" || capabilities.Limits.MaxCaseCount != 256 {
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

func TestServerRejectsCapabilitiesOutsideThePublishedV1Contract(t *testing.T) {
	for name, mutate := range map[string]func(*Capabilities){
		"invalid language id": func(value *Capabilities) { value.Languages[0].ID = "C++" },
		"empty display name":  func(value *Capabilities) { value.Languages[0].DisplayName = "" },
		"empty runtime":       func(value *Capabilities) { value.Languages[0].Runtime = "" },
		"zero bundle limit":   func(value *Capabilities) { value.Limits.MaxBundleBytes = 0 },
		"zero case limit":     func(value *Capabilities) { value.Limits.MaxCaseBytes = 0 },
		"zero case count":     func(value *Capabilities) { value.Limits.MaxCaseCount = 0 },
		"excess case count":   func(value *Capabilities) { value.Limits.MaxCaseCount = 257 },
	} {
		t.Run(name, func(t *testing.T) {
			capabilities := testCapabilities()
			mutate(&capabilities)
			if _, err := NewServer(staticAuthenticator{}, capabilities); err == nil {
				t.Fatal("NewServer accepted capabilities outside the OpenAPI v1 contract")
			}
		})
	}
}

func TestCapabilitiesEnforceExactMaximumV1SourceBytes(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.Limits.MaxSourceBytes = maximumV1SourceBytes
	if _, err := NewServer(staticAuthenticator{}, capabilities); err != nil {
		t.Fatalf("NewServer rejected exact maximum source size: %v", err)
	}

	capabilities.Limits.MaxSourceBytes = maximumV1SourceBytes + 1
	if _, err := NewServer(staticAuthenticator{}, capabilities); err == nil {
		t.Fatal("NewServer accepted source size above the v1 maximum")
	}
}

func TestServerNormalizesNilCapabilityCollectionsToJSONArrays(t *testing.T) {
	capabilities := testCapabilities()
	capabilities.JudgeModes = nil
	capabilities.Checkers = nil
	server, err := NewServer(staticAuthenticator{principal: Principal{
		TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeCapabilitiesRead: {}},
	}}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"judgeModes":[]`) || !strings.Contains(response.Body.String(), `"checkers":[]`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func testCapabilities() Capabilities {
	return Capabilities{
		APIVersion: "v1",
		Languages:  []LanguageCapability{{ID: "cpp20", DisplayName: "C++ 20", Runtime: "gcc-15"}},
		JudgeModes: []string{"ACM"},
		Checkers:   []string{"EXACT", "TOKEN"},
		Limits: CapabilityLimits{
			MaxSourceBytes: 1 << 20,
			MaxBundleBytes: 64 << 20,
			MaxCaseBytes:   64 << 20,
			MaxCaseCount:   256,
		},
	}
}
