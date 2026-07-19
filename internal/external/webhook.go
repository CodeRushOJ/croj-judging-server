package external

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const maximumWebhookBodyBytes = 1 << 20

type WebhookDisposition string

const (
	WebhookDelivered        WebhookDisposition = "DELIVERED"
	WebhookRetry            WebhookDisposition = "RETRY"
	WebhookPermanentFailure WebhookDisposition = "PERMANENT_FAILURE"
)

type WebhookDelivery struct {
	EventID        string
	DestinationURL string
	Secret         []byte
	OccurredAt     time.Time
	Body           []byte
}

type WebhookDeliverer struct {
	transport http.RoundTripper
	timeout   time.Duration
}

func NewWebhookDeliverer(transport http.RoundTripper, timeout time.Duration) (*WebhookDeliverer, error) {
	if transport == nil || timeout <= 0 {
		return nil, fmt.Errorf("webhook HTTP transport and positive timeout are required")
	}
	return &WebhookDeliverer{transport: transport, timeout: timeout}, nil
}

func (deliverer *WebhookDeliverer) Deliver(ctx context.Context, delivery WebhookDelivery) (WebhookDisposition, error) {
	if deliverer == nil || deliverer.transport == nil {
		return WebhookPermanentFailure, fmt.Errorf("webhook deliverer is not configured")
	}
	if err := validateWebhookDelivery(delivery); err != nil {
		return WebhookPermanentFailure, err
	}
	requestContext, cancel := context.WithTimeout(ctx, deliverer.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, delivery.DestinationURL, bytes.NewReader(delivery.Body))
	if err != nil {
		return WebhookPermanentFailure, fmt.Errorf("create webhook request: %w", err)
	}
	timestamp := strconv.FormatInt(delivery.OccurredAt.Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "CodeRushOJ-Judge-Webhook/1.0")
	request.Header.Set("X-CROJ-Event-Id", delivery.EventID)
	request.Header.Set("X-CROJ-Timestamp", timestamp)
	request.Header.Set("X-CROJ-Signature", signWebhook(delivery.Secret, timestamp, delivery.Body))
	// RoundTrip deliberately bypasses http.Client's redirect machinery. A 3xx
	// is classified as permanent and the signed body is never forwarded.
	response, err := deliverer.transport.RoundTrip(request)
	if err != nil {
		return WebhookRetry, fmt.Errorf("deliver webhook: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return WebhookDelivered, nil
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return WebhookRetry, nil
	default:
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			return WebhookRetry, nil
		}
		return WebhookPermanentFailure, nil
	}
}

func validateWebhookDelivery(delivery WebhookDelivery) error {
	if !externalIDPattern.MatchString(delivery.EventID) {
		return fmt.Errorf("webhook event ID is invalid")
	}
	if _, err := parseHTTPSDestination(delivery.DestinationURL); err != nil {
		return err
	}
	if len(delivery.Secret) < sha256.Size || len(delivery.Secret) > 1024 {
		return fmt.Errorf("webhook secret must contain 32 to 1024 bytes")
	}
	if delivery.OccurredAt.IsZero() || len(delivery.Body) == 0 || len(delivery.Body) > maximumWebhookBodyBytes {
		return fmt.Errorf("webhook timestamp or body is invalid")
	}
	return nil
}

func signWebhook(secret []byte, timestamp string, body []byte) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write([]byte(timestamp))
	_, _ = digest.Write([]byte("."))
	_, _ = digest.Write(body)
	return "v1=" + hex.EncodeToString(digest.Sum(nil))
}
