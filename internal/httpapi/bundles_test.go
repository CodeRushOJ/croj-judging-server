package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type bundleApplicationStub struct {
	metadata external.BundleMetadata
	replay   bool
	err      error
	uploaded []byte
	tenantID string
	key      string
	getID    string
	calls    int
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.read += count
	return count, err
}

func (application *bundleApplicationStub) UploadWithAdmission(ctx context.Context, tenantID, key string, reader io.Reader, admit external.BundleUploadAdmission) (external.BundleMetadata, bool, error) {
	application.calls++
	application.tenantID = tenantID
	application.key = key
	data, err := io.ReadAll(reader)
	if err != nil {
		return external.BundleMetadata{}, false, err
	}
	application.uploaded = data
	if err := admit(ctx, int64(len(data))); err != nil {
		return external.BundleMetadata{}, false, err
	}
	return application.metadata, application.replay, application.err
}

func (application *bundleApplicationStub) Get(_ context.Context, tenantID, bundleID string) (external.BundleMetadata, error) {
	application.calls++
	application.tenantID = tenantID
	application.getID = bundleID
	return application.metadata, application.err
}

func TestBundleUploadStreamsOneFileAndReturnsOnlyPublicMetadata(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata()}
	server := newBundleTestServer(t, ScopeBundleWrite, application)
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip-body")}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer not-returned")
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Location") != "/api/v1/bundles/aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if application.tenantID != "tenant-7" || application.key != "upload-key-00001" || !bytes.Equal(application.uploaded, []byte("zip-body")) {
		t.Fatalf("application tenant=%q key=%q body=%q", application.tenantID, application.key, application.uploaded)
	}
	var metadata external.BundleMetadata
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.BundleID != application.metadata.BundleID || strings.Contains(response.Body.String(), "object") || strings.Contains(response.Body.String(), "url") || strings.Contains(response.Body.String(), "tests.zip") {
		t.Fatalf("public response leaked storage details: %s", response.Body.String())
	}
}

func TestBundleUploadReplayReturns200(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata(), replay: true}
	server := newBundleTestServer(t, ScopeBundleWrite, application)
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBundleUploadChargesActualStreamedFileBytes(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata()}
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server := newBundleQuotaTestServer(t, application, quota)
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("exact-bundle-bytes")}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || quota.calls != 1 || quota.request.Kind != external.QuotaBundleUploadBytes || quota.request.Cost != int64(len("exact-bundle-bytes")) {
		t.Fatalf("status=%d quotaCalls=%d request=%+v body=%s", response.Code, quota.calls, quota.request, response.Body.String())
	}
}

func TestBundleUploadFailsClosedOnByteQuota(t *testing.T) {
	for name, quota := range map[string]*writeQuotaStub{
		"exceeded":    {decision: external.QuotaDecision{Allowed: false, RetryAfter: 1500 * time.Millisecond}},
		"unavailable": {err: external.ErrQuotaUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			application := &bundleApplicationStub{metadata: testBundleMetadata()}
			server := newBundleQuotaTestServer(t, application, quota)
			body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Idempotency-Key", "upload-key-00001")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			want := http.StatusTooManyRequests
			if name == "unavailable" {
				want = http.StatusServiceUnavailable
			}
			if response.Code != want || response.Header().Get("Retry-After") == "" || len(application.uploaded) == 0 {
				t.Fatalf("status=%d headers=%v uploaded=%d body=%s", response.Code, response.Header(), len(application.uploaded), response.Body.String())
			}
		})
	}
}

func TestBundleUploadRejectsAmbiguousIdempotencyHeaders(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata()}
	server := newBundleTestServer(t, ScopeBundleWrite, application)
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Add("Idempotency-Key", "upload-key-00001")
	request.Header.Add("Idempotency-Key", "upload-key-00002")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || application.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, application.calls, response.Body.String())
	}
}

func TestBundleUploadRejectsMissingKeyCallerStorageFieldsAndExtraParts(t *testing.T) {
	for name, test := range map[string]struct {
		values []multipartValue
		key    string
	}{
		"missing idempotency key": {values: []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}}},
		"caller object key":       {values: []multipartValue{{name: "objectKey", body: []byte("external/attacker.zip")}}, key: "upload-key-00001"},
		"caller URL":              {values: []multipartValue{{name: "url", body: []byte("http://169.254.169.254/")}}, key: "upload-key-00001"},
		"extra part":              {values: []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}, {name: "url", body: []byte("https://attacker")}}, key: "upload-key-00001"},
	} {
		t.Run(name, func(t *testing.T) {
			application := &bundleApplicationStub{metadata: testBundleMetadata()}
			server := newBundleTestServer(t, ScopeBundleWrite, application)
			body, contentType := multipartBody(t, test.values)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Idempotency-Key", test.key)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "169.254") || strings.Contains(response.Body.String(), "attacker") || strings.Contains(response.Body.String(), "external/") {
				t.Fatalf("problem leaked caller storage input: %s", response.Body.String())
			}
		})
	}
}

func TestBundleUploadRejectsExtraPartWithoutDrainingItsBody(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata()}
	server := newBundleTestServer(t, ScopeBundleWrite, application)
	body, contentType := multipartBody(t, []multipartValue{
		{name: "bundle", filename: "tests.zip", body: []byte("zip")},
		{name: "url", body: bytes.Repeat([]byte("x"), 1<<20)},
	})
	counted := &countingReader{reader: bytes.NewReader(body)}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", counted)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if counted.read >= len(body)/2 {
		t.Fatalf("handler drained untrusted extra part: read=%d total=%d", counted.read, len(body))
	}
}

func TestBundleUploadBoundsTheWholeMultipartEnvelope(t *testing.T) {
	application := &bundleApplicationStub{metadata: testBundleMetadata()}
	capabilities := testCapabilities()
	capabilities.Limits.MaxBundleBytes = 64
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeBundleWrite: {}}}}, capabilities,
		WithBundleApplication(application), WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: 64, RefillPeriod: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: bytes.Repeat([]byte("x"), 2<<20)}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(application.uploaded) != 0 {
		t.Fatalf("oversize multipart was accepted: uploaded=%d", len(application.uploaded))
	}
}

func TestBundleUploadMapsBoundedAndIdempotencyFailures(t *testing.T) {
	for name, test := range map[string]struct {
		err       error
		want      int
		wantRetry bool
	}{
		"too large":         {err: external.ErrBundleTooLarge, want: http.StatusRequestEntityTooLarge},
		"invalid ZIP":       {err: external.ErrInvalidBundle, want: http.StatusBadRequest},
		"key conflict":      {err: external.ErrIdempotencyConflict, want: http.StatusConflict},
		"store unavailable": {err: errors.New("minio unavailable"), want: http.StatusServiceUnavailable},
		"publishing":        {err: external.ErrBundlePublishing, want: http.StatusServiceUnavailable, wantRetry: true},
	} {
		t.Run(name, func(t *testing.T) {
			application := &bundleApplicationStub{err: test.err}
			server := newBundleTestServer(t, ScopeBundleWrite, application)
			body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Idempotency-Key", "upload-key-00001")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.want || !strings.Contains(response.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.wantRetry && response.Header().Get("Retry-After") == "" {
				t.Fatal("retryable publication response omitted Retry-After")
			}
		})
	}
}

func TestBundleMetadataIsTenantScopedAndReturns404AcrossTenants(t *testing.T) {
	application := &bundleApplicationStub{err: external.ErrBundleNotFound}
	server := newBundleTestServer(t, ScopeBundleRead, application)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/bundles/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || application.tenantID != "tenant-7" || application.getID != "aaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("status=%d tenant=%q id=%q body=%s", response.Code, application.tenantID, application.getID, response.Body.String())
	}
}

func TestBundleEndpointsRequireTheirOwnScopes(t *testing.T) {
	application := &bundleApplicationStub{}
	server := newBundleTestServer(t, ScopeCapabilitiesRead, application)
	body, contentType := multipartBody(t, []multipartValue{{name: "bundle", filename: "tests.zip", body: []byte("zip")}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/bundles", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Idempotency-Key", "upload-key-00001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || application.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, application.calls)
	}
}

func newBundleTestServer(t *testing.T, scope Scope, application BundleApplication) *Server {
	t.Helper()
	capabilities := testCapabilities()
	quota := &writeQuotaStub{decision: external.QuotaDecision{Allowed: true}}
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{scope: {}}}}, capabilities,
		WithBundleApplication(application), WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func newBundleQuotaTestServer(t *testing.T, application BundleApplication, quota external.Quota) *Server {
	t.Helper()
	capabilities := testCapabilities()
	server, err := NewServer(staticAuthenticator{principal: Principal{TenantID: "tenant-7", scopes: map[Scope]struct{}{ScopeBundleWrite: {}}}}, capabilities,
		WithBundleApplication(application), WithBundleWriteQuota(quota, external.QuotaLimit{Capacity: capabilities.Limits.MaxBundleBytes, RefillPeriod: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type multipartValue struct {
	name     string
	filename string
	body     []byte
}

func multipartBody(t *testing.T, values []multipartValue) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, value := range values {
		var part io.Writer
		var err error
		if value.filename != "" {
			part, err = writer.CreateFormFile(value.name, value.filename)
		} else {
			part, err = writer.CreateFormField(value.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(value.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func testBundleMetadata() external.BundleMetadata {
	return external.BundleMetadata{
		BundleID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", SHA256: strings.Repeat("a", 64), SizeBytes: 123,
		CaseCount: 2, ManifestVersion: 1, CreatedAt: time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC),
	}
}
