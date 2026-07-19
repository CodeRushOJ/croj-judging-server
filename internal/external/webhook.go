package external

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maximumWebhookBodyBytes  = 1 << 20
	maximumWebhookRetryAfter = 15 * time.Minute
)

type WebhookDisposition string

const (
	WebhookDelivered        WebhookDisposition = "DELIVERED"
	WebhookRetry            WebhookDisposition = "RETRY"
	WebhookPermanentFailure WebhookDisposition = "PERMANENT_FAILURE"
)

const (
	WebhookErrorConfiguration     = "configuration"
	WebhookErrorCallbackDecrypt   = "callback_decrypt"
	WebhookErrorHTTPPermanent     = "http_permanent"
	WebhookErrorHTTPRetryable     = "http_retryable"
	WebhookErrorInvalidDelivery   = "invalid_delivery"
	WebhookErrorNetwork           = "network"
	WebhookErrorUnsafeDestination = "unsafe_destination"
)

type WebhookOutcome struct {
	Disposition WebhookDisposition
	HTTPStatus  int
	RetryAfter  time.Duration
	ErrorCode   string
	err         error
}

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
	outcome := deliverer.DeliverOutcome(ctx, delivery, maximumWebhookRetryAfter)
	return outcome.Disposition, outcome.err
}

func (deliverer *WebhookDeliverer) DeliverOutcome(ctx context.Context, delivery WebhookDelivery, retryAfterMaximum time.Duration) WebhookOutcome {
	if deliverer == nil || deliverer.transport == nil {
		return permanentWebhookOutcome(WebhookErrorConfiguration, fmt.Errorf("webhook deliverer is not configured"))
	}
	if deliverer.now == nil || retryAfterMaximum <= 0 || retryAfterMaximum > maximumWebhookRetryAfter {
		return permanentWebhookOutcome(WebhookErrorConfiguration, fmt.Errorf("webhook delivery configuration is invalid"))
	}
	if err := validateWebhookDelivery(delivery); err != nil {
		return permanentWebhookOutcome(WebhookErrorInvalidDelivery, err)
	}
	requestContext, cancel := context.WithTimeout(ctx, deliverer.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, delivery.DestinationURL, bytes.NewReader(delivery.Body))
	if err != nil {
		return permanentWebhookOutcome(WebhookErrorInvalidDelivery, fmt.Errorf("create webhook request: %w", err))
	}
	requestTime := deliverer.now().UTC()
	timestamp := strconv.FormatInt(requestTime.Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "CodeRushOJ-Judge-Webhook/1.0")
	request.Header.Set("X-CodeRushOJ-Event-Id", delivery.EventID)
	request.Header.Set("X-CodeRushOJ-Timestamp", timestamp)
	request.Header.Set("X-CodeRushOJ-Signature", signWebhook(delivery.Secret, delivery.EventID, timestamp, delivery.Body))
	// RoundTrip deliberately bypasses http.Client's redirect machinery. A 3xx
	// is classified as permanent and the signed body is never forwarded.
	response, err := deliverer.transport.RoundTrip(request)
	if err != nil {
		if errors.Is(err, ErrUnsafeCallbackDestination) {
			return permanentWebhookOutcome(WebhookErrorUnsafeDestination, fmt.Errorf("deliver webhook: %w", err))
		}
		return WebhookOutcome{
			Disposition: WebhookRetry,
			ErrorCode:   WebhookErrorNetwork,
			err:         fmt.Errorf("deliver webhook: %w", err),
		}
	}
	if response == nil {
		return WebhookOutcome{
			Disposition: WebhookRetry,
			ErrorCode:   WebhookErrorNetwork,
			err:         fmt.Errorf("deliver webhook: transport returned no response"),
		}
	}
	responseTime := deliverer.now().UTC()
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode > 599 {
		return permanentWebhookOutcome(WebhookErrorInvalidDelivery, fmt.Errorf("webhook response status is invalid"))
	}
	outcome := WebhookOutcome{HTTPStatus: response.StatusCode}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		outcome.Disposition = WebhookDelivered
		return outcome
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		outcome.Disposition = WebhookRetry
		outcome.ErrorCode = WebhookErrorHTTPRetryable
	default:
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			outcome.Disposition = WebhookRetry
			outcome.ErrorCode = WebhookErrorHTTPRetryable
		} else {
			outcome.Disposition = WebhookPermanentFailure
			outcome.ErrorCode = WebhookErrorHTTPPermanent
		}
	}
	if outcome.Disposition == WebhookRetry {
		outcome.RetryAfter = parseWebhookRetryAfter(response.Header.Get("Retry-After"), responseTime, retryAfterMaximum)
	}
	return outcome
}

func permanentWebhookOutcome(code string, err error) WebhookOutcome {
	return WebhookOutcome{Disposition: WebhookPermanentFailure, ErrorCode: code, err: err}
}

func parseWebhookRetryAfter(raw string, now time.Time, maximum time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(raw, 10, 64); err == nil {
		maximumDurationSeconds := uint64(time.Duration(1<<63-1) / time.Second)
		if seconds > maximumDurationSeconds {
			return 0
		}
		maximumSeconds := uint64(maximum / time.Second)
		if seconds > maximumSeconds {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	date, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	delay := date.Sub(now)
	if delay <= 0 {
		return 0
	}
	if delay > maximum {
		return maximum
	}
	return delay
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
