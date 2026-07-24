package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	submitDeadline  time.Time
	blockSubmit     bool
}

type writeQuotaStub struct {
	decision external.QuotaDecision
	err      error
	request  external.QuotaRequest
	calls    int
	deadline time.Time
	block    bool
}

type gatedReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
	reads   int
}

func (reader *gatedReader) Read(buffer []byte) (int, error) {
	reader.reads++
	reader.once.Do(func() { close(reader.started) })
	<-reader.release
	return reader.reader.Read(buffer)
}

type readDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

type trackingBody struct {
	reader io.Reader
	closed bool
}

func (body *trackingBody) Read(buffer []byte) (int, error) { return body.reader.Read(buffer) }
func (body *trackingBody) Close() error {
	body.closed = true
	return nil
}

type readTrapBody struct {
	reads  int
	closed bool
}

func (body *readTrapBody) Read([]byte) (int, error) {
	body.reads++
	return 0, errors.New("body must not be drained without a socket read deadline")
}
func (body *readTrapBody) Close() error {
	body.closed = true
	return nil
}

func (recorder *readDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	recorder.deadlines = append(recorder.deadlines, deadline)
	return nil
}

func (quota *writeQuotaStub) Allow(ctx context.Context, request external.QuotaRequest) (external.QuotaDecision, error) {
	quota.calls++
	quota.request = request
	quota.deadline, _ = ctx.Deadline()
	if quota.block {
		<-ctx.Done()
		return external.QuotaDecision{}, ctx.Err()
	}
	return quota.decision, quota.err
}

func (service *jobServiceStub) Submit(ctx context.Context, tenantID, idempotencyKey string, command SubmitJobCommand, admit JobAdmission) (JobView, bool, error) {
	service.idempotencyKey, service.command = idempotencyKey, command
	service.submitDeadline, _ = ctx.Deadline()
	if service.err == nil && !service.replayed {
		calls := service.admissionCalls
		if calls == 0 {
			calls = 1
		}
		for index := 0; index < calls; index++ {
			if err := admit(ctx); err != nil {
				return JobView{}, false, err
			}
		}
	}
	if service.blockSubmit {
		<-ctx.Done()
		return JobView{}, false, ctx.Err()
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
	body := `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"secret source","stopOnFailure":true,"callbackId":"ceirceirceirceirceirceircg","clientReference":"submission-42"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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
		"missing key":   {"", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`},
		"unknown field": {"submission-00000042", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x","limits":{"cpu":99}}`},
		"trailing JSON": {"submission-00000042", `{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}{}`},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer valid")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", test.key)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSubmitJudgeJobRequiresOneApplicationJSONContentTypeBeforeReadingBody(t *testing.T) {
	for name, values := range map[string][]string{
		"missing":   nil,
		"wrong":     {"text/plain"},
		"repeated":  {"application/json", "application/json"},
		"malformed": {"application/json; charset"},
	} {
		t.Run(name, func(t *testing.T) {
			service := &jobServiceStub{}
			server := newJobTestServer(t, service, ScopeJobSubmit)
			body := &countingReader{reader: strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`)}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", body)
			request.Header.Set("Authorization", "Bearer valid")
			request.Header.Set("Idempotency-Key", "submission-00000042")
			request.Header["Content-Type"] = append([]string(nil), values...)
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != http.StatusUnsupportedMediaType || service.submittedTenant != "" {
				t.Fatalf("status=%d bytesRead=%d submitTenant=%q body=%s", response.Code, body.read, service.submittedTenant, response.Body.String())
			}
		})
	}
}

func TestSubmitJudgeJobKeepsShortDeadlineAndClosesConnectionForUnreadRejectedBody(t *testing.T) {
	for name, test := range map[string]struct {
		contentType string
		payload     string
	}{
		"invalid header": {"text/plain", strings.Repeat("x", 1<<20)},
		"malformed JSON": {"application/json", "!" + strings.Repeat("x", 1<<20)},
	} {
		t.Run(name, func(t *testing.T) {
			service := &jobServiceStub{}
			server := newJobTestServer(t, service, ScopeJobSubmit)
			body := &trackingBody{reader: strings.NewReader(test.payload)}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", body)
			request.Header.Set("Authorization", "Bearer valid")
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Idempotency-Key", "submission-00000042")
			response := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

			server.ServeHTTP(response, request)

			if !body.closed {
				t.Fatal("rejected authenticated body was not closed")
			}
			if len(response.deadlines) != 1 || response.deadlines[0].IsZero() {
				t.Fatalf("read deadlines=%v, want only the short non-zero deadline while unread bytes remain", response.deadlines)
			}
			if response.Header().Get("Connection") != "close" {
				t.Fatalf("Connection=%q, want close for unread rejected body", response.Header().Get("Connection"))
			}
		})
	}
}

func TestSubmitJudgeJobDoesNotDrainWhenResponseControllerCannotSetReadDeadline(t *testing.T) {
	server := newJobTestServer(t, &jobServiceStub{}, ScopeJobSubmit)
	body := &readTrapBody{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", body)
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnsupportedMediaType || body.reads != 0 || !body.closed ||
		response.Header().Get("Connection") != "close" {
		t.Fatalf("status=%d reads=%d closed=%v headers=%v", response.Code, body.reads, body.closed, response.Header())
	}
}

func TestSubmitJudgeJobUnreadRejectedBodyClosesRealHTTP1Connection(t *testing.T) {
	server := newJobTestServer(t, &jobServiceStub{}, ScopeJobSubmit)
	if err := WithJobBodyProtection(time.Second, 1)(server); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.EnableHTTP2 = false
	httpServer.Start()
	defer httpServer.Close()
	connection, err := net.Dial("tcp", httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	const bodyBytes = 1 << 20
	if _, err := fmt.Fprintf(connection, "POST /api/v1/judge-jobs HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer valid\r\nContent-Type: text/plain\r\nIdempotency-Key: submission-00000042\r\nContent-Length: %d\r\n\r\n", bodyBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, strings.Repeat("x", bodyBytes)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnsupportedMediaType || !response.Close {
		t.Fatalf("status=%d close=%v headers=%v", response.StatusCode, response.Close, response.Header)
	}
}

func TestSubmitJudgeJobAppliesDedicatedShortBodyDeadline(t *testing.T) {
	service := &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	if err := WithJobBodyProtection(2*time.Minute, 2)(server); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(response.deadlines) != 2 || response.deadlines[0].Before(started.Add(119*time.Second)) || !response.deadlines[1].IsZero() {
		t.Fatalf("read deadlines = %v, want a two-minute job-body deadline followed by reset", response.deadlines)
	}
}

func TestSubmitJudgeJobRejectsExcessBodyReaderBeforeReading(t *testing.T) {
	service := &jobServiceStub{view: JobView{JobID: "ceirceirceirceirceirceirce", Status: JobQueued}}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	if err := WithJobBodyProtection(2*time.Minute, 1)(server); err != nil {
		t.Fatal(err)
	}
	firstBody := &gatedReader{
		reader:  strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"first"}`),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", firstBody)
	firstRequest.Header.Set("Authorization", "Bearer valid")
	firstRequest.Header.Set("Idempotency-Key", "submission-00000041")
	firstRequest.Header.Set("Content-Type", "application/json")
	firstResponse := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	firstDone := make(chan struct{})
	go func() {
		server.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	<-firstBody.started

	secondBody := &countingReader{reader: strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"second"}`)}
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", secondBody)
	secondRequest.Header.Set("Authorization", "Bearer valid")
	secondRequest.Header.Set("Idempotency-Key", "submission-00000042")
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	server.ServeHTTP(secondResponse, secondRequest)

	if secondResponse.Code != http.StatusServiceUnavailable || secondResponse.Header().Get("Retry-After") == "" || secondBody.read != 0 {
		t.Fatalf("status=%d retry=%q bytesRead=%d body=%s", secondResponse.Code, secondResponse.Header().Get("Retry-After"), secondBody.read, secondResponse.Body.String())
	}
	close(firstBody.release)
	select {
	case <-firstDone:
		if firstResponse.Code != http.StatusAccepted {
			t.Fatalf("first status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first job body reader did not release its slot")
	}
}

func TestSubmitJudgeJobCapacityRejectionStillAppliesShortReadDeadline(t *testing.T) {
	server := newJobTestServer(t, &jobServiceStub{}, ScopeJobSubmit)
	if err := WithJobBodyProtection(2*time.Minute, 1)(server); err != nil {
		t.Fatal(err)
	}
	server.jobBodyReaders <- struct{}{}
	defer func() { <-server.jobBodyReaders }()
	body := &trackingBody{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", body)
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || len(response.deadlines) != 2 ||
		response.deadlines[0].IsZero() || response.deadlines[1].IsZero() ||
		!response.deadlines[1].Before(response.deadlines[0]) ||
		!body.closed || response.Header().Get("Connection") != "close" {
		t.Fatalf("status=%d deadlines=%v closed=%v headers=%v", response.Code, response.deadlines, body.closed, response.Header())
	}
}

func TestSubmitJudgeJobCapacityRejectionDoesNotDrainSlowUnreadHTTP1Body(t *testing.T) {
	server := newJobTestServer(t, &jobServiceStub{}, ScopeJobSubmit)
	server.jobBodyReadTimeout = 2 * time.Second
	server.jobBodyReaders = make(chan struct{}, 1)
	server.jobBodyReaders <- struct{}{}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.EnableHTTP2 = false
	httpServer.Start()
	defer httpServer.Close()

	connection, err := net.Dial("tcp", httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(750 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST /api/v1/judge-jobs HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer valid\r\nContent-Type: application/json\r\nIdempotency-Key: submission-00000042\r\nContent-Length: 32\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("capacity rejection waited for unread body: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("capacity rejection took %s, want an immediate response", elapsed)
	}
	if response.StatusCode != http.StatusServiceUnavailable || !response.Close {
		t.Fatalf("status=%d close=%v headers=%v", response.StatusCode, response.Close, response.Header)
	}
}

func TestSubmitJudgeJobBodyReadTimeoutReturnsRetryable408(t *testing.T) {
	server := newJobTestServer(t, &jobServiceStub{}, ScopeJobSubmit)
	server.jobBodyReadTimeout = 50 * time.Millisecond
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.EnableHTTP2 = false
	httpServer.Start()
	defer httpServer.Close()

	connection, err := net.Dial("tcp", httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST /api/v1/judge-jobs HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer valid\r\nContent-Type: application/json\r\nIdempotency-Key: submission-00000042\r\nContent-Length: 128\r\n\r\n{\"bundleId\":\""); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var problem Problem
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusRequestTimeout || problem.Status != http.StatusRequestTimeout ||
		response.Header.Get("Retry-After") == "" || !response.Close {
		t.Fatalf("status=%d close=%v headers=%v problem=%+v", response.StatusCode, response.Close, response.Header, problem)
	}
}

func TestSubmitJudgeJobRejectsMultipleIdempotencyHeaders(t *testing.T) {
	service := &jobServiceStub{}
	server := newJobTestServer(t, service, ScopeJobSubmit)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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
	body, err := json.Marshal(SubmitJobCommand{BundleID: "ceirceirceirceirceirceircf", Language: "cpp", SourceCode: source})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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

func TestListJudgeJobsMapsInvalidRepositoryCursorToBadRequest(t *testing.T) {
	service := &jobServiceStub{err: ErrJobInvalid}
	server := newJobTestServer(t, service, ScopeJobRead)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/judge-jobs?cursor=tampered", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid-list-query") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"x"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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
	request.Header.Set("Content-Type", "application/json")
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
	request.Header.Set("Content-Type", "application/json")
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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"same source"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
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
	request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"source"}`))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "submission-00000042")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || quota.calls != 1 {
		t.Fatalf("status=%d quotaCalls=%d body=%s", response.Code, quota.calls, response.Body.String())
	}
}

func TestSubmitJudgeJobBoundsAdmissionAndRepositoryWithOneApplicationDeadline(t *testing.T) {
	for name, test := range map[string]struct {
		service *jobServiceStub
		quota   *writeQuotaStub
	}{
		"admission": {
			service: &jobServiceStub{},
			quota:   &writeQuotaStub{block: true},
		},
		"repository": {
			service: &jobServiceStub{blockSubmit: true},
			quota:   &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := newJobQuotaTestServer(t, test.service, test.quota)
			if err := WithJobSubmitTimeout(40 * time.Millisecond)(server); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/judge-jobs", strings.NewReader(`{"bundleId":"ceirceirceirceirceirceircf","language":"cpp","sourceCode":"source"}`))
			request.Header.Set("Authorization", "Bearer valid")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "submission-00000042")
			response := httptest.NewRecorder()
			started := time.Now()

			server.ServeHTTP(response, request)

			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("blocked %s exceeded application deadline: %s", name, elapsed)
			}
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
			if test.service.submitDeadline.IsZero() && name == "repository" {
				t.Fatal("repository did not receive the submit application deadline")
			}
			if test.quota.deadline.IsZero() {
				t.Fatal("admission did not receive the submit application deadline")
			}
			if name == "repository" && !test.service.submitDeadline.Equal(test.quota.deadline) {
				t.Fatalf("admission deadline=%s repository deadline=%s", test.quota.deadline, test.service.submitDeadline)
			}
		})
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
