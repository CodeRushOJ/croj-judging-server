package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
}

func (service *jobServiceStub) Submit(_ context.Context, tenantID, idempotencyKey string, command SubmitJobCommand) (JobView, bool, error) {
	service.submittedTenant, service.idempotencyKey, service.command = tenantID, idempotencyKey, command
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

func newJobTestServer(t *testing.T, service JobService, scopes ...Scope) *Server {
	t.Helper()
	granted := make(map[Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		granted[scope] = struct{}{}
	}
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: granted}}, testCapabilities(), WithJobService(service))
	if err != nil {
		t.Fatal(err)
	}
	return server
}
