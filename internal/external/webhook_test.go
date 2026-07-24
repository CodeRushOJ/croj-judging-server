package external

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type webhookDoerFunc func(*http.Request) (*http.Response, error)

func (function webhookDoerFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestWebhookDeliverySignsExactBodyAndRequiredHeaders(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 34, 56, 0, time.UTC)
	secret := []byte("0123456789abcdef0123456789abcdef")
	body := []byte(`{"eventId":"ceirceirceirceirceirceirce","type":"judge.job.completed"}`)
	var captured []byte
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(request *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.String() != "https://oj.example.com/hooks/croj" {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if request.Header.Get("X-CodeRushOJ-Event-Id") != "ceirceirceirceirceirceirce" || request.Header.Get("X-CodeRushOJ-Timestamp") != "1784464496" {
			t.Fatalf("headers=%#v", request.Header)
		}
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte("v1\n26\nceirceirceirceirceirceirce\n1784464496\n"))
		_, _ = mac.Write(captured)
		want := "v1=" + hex.EncodeToString(mac.Sum(nil))
		if request.Header.Get("X-CodeRushOJ-Signature") != want {
			t.Fatalf("signature=%q want=%q", request.Header.Get("X-CodeRushOJ-Signature"), want)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deliverer.now = func() time.Time { return now }
	disposition, err := deliverer.Deliver(context.Background(), WebhookDelivery{
		EventID: "ceirceirceirceirceirceirce", DestinationURL: "https://oj.example.com/hooks/croj",
		Secret: secret, Body: body,
	})
	if err != nil || disposition != WebhookDelivered || string(captured) != string(body) {
		t.Fatalf("disposition=%q err=%v captured=%q", disposition, err, captured)
	}
}

func TestWebhookDeliveryClassifiesHTTPStatusWithoutFollowingRedirects(t *testing.T) {
	for _, test := range []struct {
		status      int
		disposition WebhookDisposition
	}{
		{200, WebhookDelivered}, {204, WebhookDelivered},
		{301, WebhookPermanentFailure}, {400, WebhookPermanentFailure}, {401, WebhookPermanentFailure}, {409, WebhookPermanentFailure},
		{408, WebhookRetry}, {425, WebhookRetry}, {429, WebhookRetry}, {500, WebhookRetry}, {503, WebhookRetry},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 10_000)))}, nil
			}), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			disposition, err := deliverer.Deliver(context.Background(), validWebhookDelivery())
			if err != nil || disposition != test.disposition {
				t.Fatalf("disposition=%q err=%v", disposition, err)
			}
		})
	}
}

func TestWebhookDeliveryTreatsAuthorityMismatchAsPermanent(t *testing.T) {
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
		return nil, ErrUnsafeCallbackDestination
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := deliverer.Deliver(context.Background(), validWebhookDelivery())
	if disposition != WebhookPermanentFailure || !errors.Is(err, ErrUnsafeCallbackDestination) {
		t.Fatalf("disposition=%q err=%v", disposition, err)
	}
}

func TestWebhookDeliveryRejectsUnsafeOrSecretLeakingInputs(t *testing.T) {
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe request reached the network")
		return nil, nil
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*WebhookDelivery){
		"http":         func(value *WebhookDelivery) { value.DestinationURL = "http://oj.example.com/hook" },
		"userinfo":     func(value *WebhookDelivery) { value.DestinationURL = "https://user:pass@oj.example.com/hook" },
		"fragment":     func(value *WebhookDelivery) { value.DestinationURL = "https://oj.example.com/hook#secret" },
		"short secret": func(value *WebhookDelivery) { value.Secret = []byte("short") },
		"bad event":    func(value *WebhookDelivery) { value.EventID = "../event" },
		"mismatched event": func(value *WebhookDelivery) {
			value.Body = []byte(`{"eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		},
		"duplicate event": func(value *WebhookDelivery) {
			value.Body = []byte(`{"eventId":"ceirceirceirceirceirceirce","eventId":"ceirceirceirceirceirceirce"}`)
		},
		"empty body": func(value *WebhookDelivery) { value.Body = nil },
	} {
		t.Run(name, func(t *testing.T) {
			value := validWebhookDelivery()
			mutate(&value)
			if _, err := deliverer.Deliver(context.Background(), value); err == nil || strings.Contains(err.Error(), string(value.Secret)) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestWebhookSignatureBindsEventIdentity(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	body := []byte(`{"eventId":"ceirceirceirceirceirceirce"}`)
	first := signWebhook(secret, "ceirceirceirceirceirceirce", "1784464496", body)
	second := signWebhook(secret, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "1784464496", body)
	if first == second {
		t.Fatal("changing the deduplication identity did not invalidate the signature")
	}
}

func validWebhookDelivery() WebhookDelivery {
	return WebhookDelivery{
		EventID: "ceirceirceirceirceirceirce", DestinationURL: "https://oj.example.com/hook",
		Secret: []byte("0123456789abcdef0123456789abcdef"), Body: []byte(`{"eventId":"ceirceirceirceirceirceirce","eventType":"judge.job.completed"}`),
	}
}

func TestWebhookRetrySignatureUsesFreshAttemptTimestamp(t *testing.T) {
	var timestamps []string
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(request *http.Request) (*http.Response, error) {
		timestamps = append(timestamps, request.Header.Get("X-CodeRushOJ-Timestamp"))
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	deliverer.now = func() time.Time { return now }
	if _, err := deliverer.Deliver(context.Background(), validWebhookDelivery()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if _, err := deliverer.Deliver(context.Background(), validWebhookDelivery()); err != nil {
		t.Fatal(err)
	}
	if len(timestamps) != 2 || timestamps[0] == timestamps[1] {
		t.Fatalf("retry signatures reused stale timestamps: %v", timestamps)
	}
}

func TestWebhookOutcomeClassifiesEveryHTTPStatus(t *testing.T) {
	for status := 200; status <= 599; status++ {
		want := WebhookPermanentFailure
		wantCode := WebhookErrorHTTPPermanent
		switch {
		case status >= 200 && status <= 299:
			want = WebhookDelivered
			wantCode = ""
		case status == http.StatusRequestTimeout, status == http.StatusTooEarly, status == http.StatusTooManyRequests:
			want = WebhookRetry
			wantCode = WebhookErrorHTTPRetryable
		case status >= 500:
			want = WebhookRetry
			wantCode = WebhookErrorHTTPRetryable
		}
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			const responseSecret = "response-body-must-not-leak"
			deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader(responseSecret)),
				}, nil
			}), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), 15*time.Minute)
			if outcome.Disposition != want || outcome.HTTPStatus != status || outcome.ErrorCode != wantCode {
				t.Fatalf("status=%d outcome=%+v want disposition=%q code=%q", status, outcome, want, wantCode)
			}
			if strings.Contains(outcome.ErrorCode, responseSecret) {
				t.Fatalf("response body leaked in outcome: %+v", outcome)
			}
		})
	}
}

func TestWebhookOutcomeTreatsFinalInformationalResponseAsInvalidTerminalResult(t *testing.T) {
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: io.NopCloser(strings.NewReader("ignored"))}, nil
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), 15*time.Minute)
	if outcome.Disposition != WebhookPermanentFailure || outcome.HTTPStatus != 0 || outcome.ErrorCode != WebhookErrorInvalidDelivery {
		t.Fatalf("informational outcome=%+v", outcome)
	}
	if !validWebhookSettlement(WebhookSettlement{
		Disposition: outcome.Disposition, HTTPStatus: outcome.HTTPStatus, ErrorCode: outcome.ErrorCode,
	}) {
		t.Fatalf("repository rejected deliverer informational outcome=%+v", outcome)
	}
}

func TestWebhookOutcomeClassifiesNetworkAndUnsafeFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		deliveryErr error
		want        WebhookDisposition
		wantCode    string
	}{
		{name: "resolver outage", deliveryErr: errors.New("temporary DNS failure"), want: WebhookRetry, wantCode: WebhookErrorNetwork},
		{name: "private DNS answer", deliveryErr: fmt.Errorf("dial: %w", ErrUnsafeCallbackDestination), want: WebhookPermanentFailure, wantCode: WebhookErrorUnsafeDestination},
		{name: "authority mismatch", deliveryErr: ErrUnsafeCallbackDestination, want: WebhookPermanentFailure, wantCode: WebhookErrorUnsafeDestination},
	} {
		t.Run(test.name, func(t *testing.T) {
			deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
				return nil, test.deliveryErr
			}), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), 15*time.Minute)
			if outcome.Disposition != test.want || outcome.HTTPStatus != 0 || outcome.RetryAfter != 0 || outcome.ErrorCode != test.wantCode {
				t.Fatalf("outcome=%+v", outcome)
			}
		})
	}
}

func TestWebhookRetryAfterAcceptsDeltaAndDateAndClamps(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 11, 12, 0, time.UTC)
	for _, test := range []struct {
		name    string
		header  string
		status  int
		maximum time.Duration
		want    time.Duration
	}{
		{name: "delta", header: "120", status: http.StatusTooManyRequests, maximum: 15 * time.Minute, want: 2 * time.Minute},
		{name: "http date", header: now.Add(7 * time.Minute).Format(http.TimeFormat), status: http.StatusServiceUnavailable, maximum: 15 * time.Minute, want: 7 * time.Minute},
		{name: "clamp delta", header: "3600", status: http.StatusTooManyRequests, maximum: 15 * time.Minute, want: 15 * time.Minute},
		{name: "caller lower cap", header: "600", status: http.StatusServiceUnavailable, maximum: 3 * time.Minute, want: 3 * time.Minute},
		{name: "ignore successful response", header: "120", status: http.StatusNoContent, maximum: 15 * time.Minute, want: 0},
		{name: "ignore permanent response", header: "120", status: http.StatusBadRequest, maximum: 15 * time.Minute, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.status,
					Header:     http.Header{"Retry-After": []string{test.header}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			deliverer.now = func() time.Time { return now }
			outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), test.maximum)
			if outcome.RetryAfter != test.want {
				t.Fatalf("outcome=%+v want retry-after=%s", outcome, test.want)
			}
		})
	}
}

func TestWebhookRetryAfterIgnoresInvalidAndPastValues(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 11, 12, 0, time.UTC)
	for _, header := range []string{"", "-1", "+1", "1.5", "tomorrow", "18446744073709551615", now.Add(-time.Minute).Format(http.TimeFormat)} {
		t.Run(header, func(t *testing.T) {
			deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": []string{header}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			deliverer.now = func() time.Time { return now }
			outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), 15*time.Minute)
			if outcome.Disposition != WebhookRetry || outcome.RetryAfter != 0 {
				t.Fatalf("header=%q outcome=%+v", header, outcome)
			}
		})
	}
}

func TestWebhookRetryAfterDateUsesResponseTime(t *testing.T) {
	requestTime := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	responseTime := requestTime.Add(10 * time.Minute)
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Retry-After": []string{responseTime.Add(2 * time.Minute).Format(http.TimeFormat)}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clockCalls := 0
	deliverer.now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return requestTime
		}
		return responseTime
	}
	outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), 15*time.Minute)
	if outcome.RetryAfter != 2*time.Minute || clockCalls != 2 {
		t.Fatalf("outcome=%+v clock calls=%d", outcome, clockCalls)
	}
}

func TestWebhookOutcomeRejectsInvalidRetryAfterMaximum(t *testing.T) {
	deliverer, err := newWebhookDelivererForTest(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid delivery configuration reached the network")
		return nil, nil
	}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, maximum := range []time.Duration{0, -time.Second, 15*time.Minute + time.Nanosecond} {
		outcome := deliverer.DeliverOutcome(context.Background(), validWebhookDelivery(), maximum)
		if outcome.Disposition != WebhookPermanentFailure || outcome.ErrorCode != WebhookErrorConfiguration {
			t.Fatalf("maximum=%s outcome=%+v", maximum, outcome)
		}
	}
}
