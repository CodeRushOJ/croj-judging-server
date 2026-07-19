package external

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Body           []byte
}

type WebhookDeliverer struct {
	transport http.RoundTripper
	timeout   time.Duration
	now       func() time.Time
}

func newWebhookDelivererForTest(transport http.RoundTripper, timeout time.Duration) (*WebhookDeliverer, error) {
	if transport == nil || timeout <= 0 {
		return nil, fmt.Errorf("webhook HTTP transport and positive timeout are required")
	}
	return &WebhookDeliverer{transport: transport, timeout: timeout, now: time.Now}, nil
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
	timestamp := strconv.FormatInt(deliverer.now().UTC().Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "CodeRushOJ-Judge-Webhook/1.0")
	request.Header.Set("X-CodeRushOJ-Event-Id", delivery.EventID)
	request.Header.Set("X-CodeRushOJ-Timestamp", timestamp)
	request.Header.Set("X-CodeRushOJ-Signature", signWebhook(delivery.Secret, delivery.EventID, timestamp, delivery.Body))
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
	if len(delivery.Body) == 0 || len(delivery.Body) > maximumWebhookBodyBytes {
		return fmt.Errorf("webhook body is invalid")
	}
	bodyEventID, err := webhookBodyEventID(delivery.Body)
	if err != nil || bodyEventID != delivery.EventID {
		return fmt.Errorf("webhook body event ID does not match the delivery")
	}
	return nil
}

func signWebhook(secret []byte, eventID, timestamp string, body []byte) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(digest, "v1\n%d\n%s\n%s\n", len(eventID), eventID, timestamp)
	_, _ = digest.Write(body)
	return "v1=" + hex.EncodeToString(digest.Sum(nil))
}

func webhookBodyEventID(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", fmt.Errorf("webhook body must be a JSON object")
	}
	var eventID string
	seen := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", err
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", fmt.Errorf("webhook body key is invalid")
		}
		if key == "eventId" {
			if seen || decoder.Decode(&eventID) != nil {
				return "", fmt.Errorf("webhook body eventId is duplicate or invalid")
			}
			seen = true
			continue
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return "", err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return "", fmt.Errorf("webhook body contains trailing data")
	}
	if !seen || !externalIDPattern.MatchString(eventID) {
		return "", fmt.Errorf("webhook body eventId is missing or invalid")
	}
	return eventID, nil
}
