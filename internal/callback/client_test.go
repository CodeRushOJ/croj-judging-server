package callback

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testServiceToken = "0123456789abcdef0123456789abcdef"

func TestClientPublishesAuthenticatedResult(t *testing.T) {
	var received Result
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/internal/v1/judge-results" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if token := request.Header.Get("X-CROJ-Service-Token"); token != testServiceToken {
			t.Errorf("service token = %q", token)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":20000,"message":"操作成功","data":{"disposition":"APPLIED"},"success":true}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/api", testServiceToken, time.Second, server.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	want := validResult()
	disposition, err := client.Publish(context.Background(), want)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if disposition != DispositionApplied {
		t.Fatalf("disposition = %q", disposition)
	}
	if received != want {
		t.Fatalf("received = %+v, want %+v", received, want)
	}
}

func TestClientAcceptsDuplicateAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"code":20000,"message":"操作成功","data":{"disposition":"DUPLICATE"},"success":true}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/api", testServiceToken, time.Second, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := client.Publish(context.Background(), validResult())
	if err != nil || disposition != DispositionDuplicate {
		t.Fatalf("Publish = %q, %v", disposition, err)
	}
}

func TestClientClassifiesHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooEarly, false},
		{http.StatusTooManyRequests, false},
		{http.StatusForbidden, true},
		{http.StatusConflict, true},
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/api", testServiceToken, time.Second, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Publish(context.Background(), validResult())
			if err == nil || IsPermanent(err) != test.permanent {
				t.Fatalf("Publish error = %v, permanent = %v", err, IsPermanent(err))
			}
		})
	}
}

func TestClientDoesNotForwardServiceTokenAcrossRedirect(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		redirected = true
		if token := request.Header.Get("X-CROJ-Service-Token"); token != "" {
			t.Errorf("service token was forwarded across redirect: %q", token)
		}
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := NewClient(origin.URL+"/api", testServiceToken, time.Second, origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Publish(context.Background(), validResult())
	if err == nil || IsPermanent(err) {
		t.Fatalf("redirect error = %v, permanent=%v", err, IsPermanent(err))
	}
	if redirected {
		t.Fatal("callback followed redirect")
	}
}

func TestNewClientRejectsUnsafeConfiguration(t *testing.T) {
	for name, values := range map[string][2]string{
		"missing api path": {"http://backend:7999", testServiceToken},
		"short token":      {"http://backend:7999/api", "too-short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(values[0], values[1], time.Second, http.DefaultClient); err == nil {
				t.Fatal("expected invalid configuration")
			}
		})
	}
}

func validResult() Result {
	return Result{
		ResultID:       "50f75fdf-fdea-473f-a156-bf1ed60acf58",
		SubmissionID:   99,
		AttemptNo:      1,
		Status:         StatusAccepted,
		ExitCode:       0,
		TimeUsedMillis: 12,
		MemoryUsedKB:   2048,
		Stdout:         "ok\n",
	}
}
