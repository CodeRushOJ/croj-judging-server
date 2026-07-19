package external

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	deliverer, err := NewWebhookDeliverer(webhookDoerFunc(func(request *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.String() != "https://oj.example.com/hooks/croj" {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if request.Header.Get("X-CROJ-Event-Id") != "ceirceirceirceirceirceirce" || request.Header.Get("X-CROJ-Timestamp") != "1784464496" {
			t.Fatalf("headers=%#v", request.Header)
		}
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte("1784464496."))
		_, _ = mac.Write(captured)
		want := "v1=" + hex.EncodeToString(mac.Sum(nil))
		if request.Header.Get("X-CROJ-Signature") != want {
			t.Fatalf("signature=%q want=%q", request.Header.Get("X-CROJ-Signature"), want)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := deliverer.Deliver(context.Background(), WebhookDelivery{
		EventID: "ceirceirceirceirceirceirce", DestinationURL: "https://oj.example.com/hooks/croj",
		Secret: secret, OccurredAt: now, Body: body,
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
			deliverer, err := NewWebhookDeliverer(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
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

func TestWebhookDeliveryRejectsUnsafeOrSecretLeakingInputs(t *testing.T) {
	deliverer, err := NewWebhookDeliverer(webhookDoerFunc(func(*http.Request) (*http.Response, error) {
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
		"empty body":   func(value *WebhookDelivery) { value.Body = nil },
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

func validWebhookDelivery() WebhookDelivery {
	return WebhookDelivery{
		EventID: "ceirceirceirceirceirceirce", DestinationURL: "https://oj.example.com/hook",
		Secret:     []byte("0123456789abcdef0123456789abcdef"),
		OccurredAt: time.Date(2026, 7, 19, 12, 34, 56, 0, time.UTC), Body: []byte(`{"ok":true}`),
	}
}
