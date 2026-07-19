package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type jobServiceStub struct {
	submittedTenant string
	idempotencyKey  string
	command         SubmitJobCommand
	view            JobView
	replayed        bool
	err             error
	getTenant       string
	getID           string
	cancelTenant    string
	cancelID        string
	listTenant      string
	listQuery       JobListQuery
	listPage        JobListPage
	admissionCalls  int
}

type writeQuotaStub struct {
	decision external.QuotaDecision
	err      error
	request  external.QuotaRequest
	calls    int
}

func (quota *writeQuotaStub) Allow(_ context.Context, request external.QuotaRequest) (external.QuotaDecision, error) {
	quota.calls++
	quota.request = request
	return quota.decision, quota.err
}

func (service *jobServiceStub) Submit(_ context.Context, tenantID, idempotencyKey string, command SubmitJobCommand, admit JobAdmission) (JobView, bool, error) {
	service.idempotencyKey, service.command = idempotencyKey, command
	if service.err == nil && !service.replayed {
		calls := service.admissionCalls
		if calls == 0 {
			calls = 1
		}
		for index := 0; index < calls; index++ {
			if err := admit(); err != nil {
				return JobView{}, false, err
			}
		}
	}
	service.submittedTenant = tenantID
	return service.view, service.replayed, service.err
}
func (service *jobServiceStub) Get(_ context.Context, tenantID, jobID string) (JobView, error) {
	service.getTenant, service.getID = tenantID, jobID
	return service.view, service.err
}
func (service *jobServiceStub) Cancel(_ context.Context, tenantID, jobID string) (JobView, error) {
	service.cancelTenant, service.cancelID = tenantID, jobID
	return service.view, service.err
}
func (service *jobServiceStub) List(_ context.Context, tenantID string, query JobListQuery) (JobListPage, error) {
	service.listTenant, service.listQuery = tenantID, query
	return service.listPage, service.err
}

func TestSubmitJudgeJobReturns202AndNeverEchoesSource(t *testing.T) {
	created := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	service := &jobServiceStub{view: JobView{
		JobID: "ceirceirceirceirceirceirce", Status: JobQueued,
		StatusURL: "/api/v1/judge-jobs/ceirceirceirceirceirceirce", CreatedAt: created,
		ClientReference: "submission-42",
	}}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	body := `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"secret source","stopOnFailure":true,"callbackId":"ceirceirceirceirceirceircg","clientReference":"submission-42"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Header().Get("Location") != service.view.StatusURL {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if service.submittedTenant != "tenant-7" || service.idempotencyKey != "submission-00000042" || service.command.BundleID != "ceirceirceirceirceirceircf" || service.command.SourceCode != "secret source" {
		t.Fatalf("submit call tenant=%q key=%q command=%+v", service.submittedTenant, service.idempotencyKey, service.command)
	}
	if strings.Contains(response.Body.String(), "secret source") || strings.Contains(response.Body.String(), "ceirceirceirceirceirceircf") {
		t.Fatalf("response leaked source or hidden bundle identity: %s", response.Body.String())
	}
}

func TestSubmitJudgeJobUsesStrictJSONAndRequiresIdempotencyKey(t *testing.T) {
	service := &jobServiceStub{}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	for name, test := range map[string]struct{ key, body string }{
		"missing key":   {"", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`},
		"unknown field": {"submission-00000042", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x","limits":{"cpu":99}}`},
		"trailing JSON": {"submission-00000042", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}{}`},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer valid")
			request.Header.Set("Idempotency-Key", test.key)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSubmitJudgeJobRejectsMultipleIdempotencyHeaders(t *testing.T) {
	service := &jobServiceStub{}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Add("Idempotency-Key", "submission-00000042")
	request.Header.Add("Idempotency-Key", "submission-00000043")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.submittedTenant != "" {
		t.Fatalf("status=%d submitTenant=%q body=%s", response.Code, service.submittedTenant, response.Body.String())
	}
}

func TestSubmitJudgeJobWireLimitAllowsWorstCaseEscapedSource(t *testing.T) {
	service := &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	source := strings.Repeat("\x01", 1<<20)
	body, err := json.Marshal(SubmitJobCommand{BundleID: "ceirceirceirceirceirceircf", Language: "cpp20", SourceCode: source})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || len(service.command.SourceCode) != len(source) {
		t.Fatalf("wireBytes=%d status=%d sourceBytes=%d body=%s", len(body), response.Code, len(service.command.SourceCode), response.Body.String())
	}
}

func TestListJudgeJobsUsesStableCursorQuery(t *testing.T) {
	service := &jobServiceStub{listPage: JobListPage{
		Items:      []JobView{{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}},
		NextCursor: "next-opaque-cursor",
	}}
	server := newJobTestServer(t, service, ScopeJobRead)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs?cursor=opaque-cursor&limit=25&status=RUNNING", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.listTenant != "tenant-7" || service.listQuery.Cursor != "opaque-cursor" || service.listQuery.Limit != 25 || service.listQuery.Status != JobRunning {
		t.Fatalf("list query was not tenant scoped: tenant=%q query=%+v", service.listTenant, service.listQuery)
	}
	if !strings.Contains(response.Body.String(), `"nextCursor"`) || strings.Contains(response.Body.String(), "source") {
		t.Fatalf("unexpected list response: %s", response.Body.String())
	}
}

func TestListJudgeJobsRejectsInvalidFilters(t *testing.T) {
	service := &jobServiceStub{}
	server := newJobTestServer(t, service, ScopeJobRead)
	for _, query := range []string{"limit=0", "limit=101", "limit=nope", "status=UNKNOWN", "extra=x", "limit=%ZZ"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs?"+query, nil)
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}

func TestGetAndCancelJudgeJobAreTenantScopedAndStatusAware(t *testing.T) {
	service := &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobCancelled}}
	server := newJobTestServer(t, service, ScopeJobRead, ScopeJobCancel)
	for method, path := range map[string]string{
		http.MethodGet:  "/api/v1/judge-jobs/ceirceirceirceirceirceirce",
		http.MethodPost: "/api/v1/judge-jobs/ceirceirceirceirceirceirce/cancel",
	} {
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, path, response.Code, response.Body.String())
		}
	}
	if service.getTenant != "tenant-7" || service.getID != service.view.JobID || service.cancelTenant != "tenant-7" || service.cancelID != service.view.JobID {
		t.Fatalf("get/cancel were not tenant scoped: %+v", service)
	}
}

func TestJobNotFoundIsAlways404WithoutExistenceDisclosure(t *testing.T) {
	service := &jobServiceStub{err: ErrJobNotFound}
	server := newJobTestServer(t, service, ScopeJobRead)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "tenant") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestIdempotencyConflictMapsTo409(t *testing.T) {
	service := &jobServiceStub{err: ErrIdempotencyConflict}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestQueuedQuotaMapsTo429WithoutPolicyDisclosure(t *testing.T) {
	service := &jobServiceStub{err: ErrJobQuotaExceeded}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" || strings.Contains(response.Body.String(), "maxQueuedJobs") {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestSubmitJudgeJobEnforcesDistributedQuotaBeforeCreatingJob(t *testing.T) {
	service := &jobServiceStub{}
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: false, RetryAfter: 1500 * time.Millisecond}}
	server := newJobQuotaTestServer(t, service, quota)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"sourceCode":"must-not-be-read"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if quota.request.TenantID != "tenant-7" || quota.request.Kind != external.QuotaJudgeSubmit || quota.request.Cost != 1 || service.submittedTenant != "" {
		t.Fatalf("quota=%+v submitTenant=%q", quota.request, service.submittedTenant)
	}
}

func TestSubmitJudgeJobFailsClosedWhenQuotaStateIsUnavailable(t *testing.T) {
	service := &jobServiceStub{}
	quota := &writeQuotaStub{err: external.ErrQuotaUnavailable}
	server := newJobQuotaTestServer(t, service, quota)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" || service.submittedTenant != "" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestIdempotentReplayBypassesUnavailableNewWriteQuota(t *testing.T) {
	service := &jobServiceStub{replayed: true, view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}}
	quota := &writeQuotaStub{err: external.ErrQuotaUnavailable}
	server := newJobQuotaTestServer(t, service, quota)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"same source"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Idempotent-Replay") != "true" || quota.calls != 0 {
		t.Fatalf("status=%d replay=%q quotaCalls=%d body=%s", response.Code, response.Header().Get("Idempotent-Replay"), quota.calls, response.Body.String())
	}
}

func TestJobServiceCannotDoubleChargeOneAdmission(t *testing.T) {
	service := &jobServiceStub{
		admissionCalls: 2,
		view:           JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued},
	}
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server := newJobQuotaTestServer(t, service, quota)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp20","sourceCode":"source"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || quota.calls != 1 {
		t.Fatalf("status=%d quotaCalls=%d body=%s", response.Code, quota.calls, response.Body.String())
	}
}

func TestJudgeJobReadsRemainAvailableWhenWriteQuotaIsDown(t *testing.T) {
	service := &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobRunning}}
	quota := &writeQuotaStub{err: external.ErrQuotaUnavailable}
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeJobRead: {}}}}, testCapabilities(),
		WithJobService(service), WithJobWriteQuota(quota, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs/ceirceirceirceirceirceirce", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || quota.request.TenantID != "" {
		t.Fatalf("status=%d quota=%+v body=%s", response.Code, quota.request, response.Body.String())
	}
}

func TestServerRejectsJobServiceWithoutFailClosedQuota(t *testing.T) {
	_, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7"}}, testCapabilities(), WithJobService(&jobServiceStub{}))
	if err == nil || !strings.Contains(err.Error(), "write quota") {
		t.Fatalf("error=%v", err)
	}
}

func newJobTestServer(t *testing.T, service JobService, scopes ...Scope) *Server {
	t.Helper()
	granted := make(map[Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		granted[scope] = struct{}{}
	}
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: granted}}, testCapabilities(),
		WithJobService(service), WithJobWriteQuota(quota, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func newJobQuotaTestServer(t *testing.T, service JobService, quota external.Quota) *Server {
	t.Helper()
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeJobSubmit: {}}}}, testCapabilities(),
		WithJobService(service), WithJobWriteQuota(quota, external.QuotaLimit{Capacity: 20, RefillPeriod: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}
