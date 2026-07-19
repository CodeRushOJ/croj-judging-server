package external

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

const terminalWebhookSchemaVersion = 1

type TerminalWebhookEvent struct {
	EventID    string
	OccurredAt time.Time
	Job        ExternalJobRecord
}

type terminalWebhookPayload struct {
	SchemaVersion   int               `json:"schemaVersion"`
	EventID         string            `json:"eventId"`
	EventType       string            `json:"eventType"`
	OccurredAt      string            `json:"occurredAt"`
	TenantID        string            `json:"tenantId"`
	JobID           string            `json:"jobId"`
	ClientReference string            `json:"clientReference,omitempty"`
	Status          JobStatus         `json:"status"`
	Result          *DurableJobResult `json:"result,omitempty"`
	FailureCode     string            `json:"failureCode,omitempty"`
}

func EncodeTerminalWebhookEvent(event TerminalWebhookEvent) (string, []byte, []byte, error) {
	job := event.Job
	if !externalIDPattern.MatchString(event.EventID) || event.OccurredAt.IsZero() ||
		!externalIDPattern.MatchString(job.TenantExternalID) || !externalIDPattern.MatchString(job.ExternalID) ||
		len(job.ClientReference) > 255 || !utf8.ValidString(job.ClientReference) {
		return "", nil, nil, fmt.Errorf("terminal webhook identity is invalid")
	}
	payload := terminalWebhookPayload{
		SchemaVersion: terminalWebhookSchemaVersion,
		EventID:       event.EventID, OccurredAt: event.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		TenantID: job.TenantExternalID, JobID: job.ExternalID,
		ClientReference: job.ClientReference, Status: job.Status,
	}
	switch job.Status {
	case JobStatusSucceeded:
		if job.Result == nil || job.FailureCode != "" || validateDurableJobResult(*job.Result) != nil {
			return "", nil, nil, fmt.Errorf("successful webhook result is invalid")
		}
		result := *job.Result
		result.Cases = append([]DurableCaseResult(nil), job.Result.Cases...)
		if result.Cases == nil {
			result.Cases = []DurableCaseResult{}
		}
		payload.EventType = "judge.job.completed"
		payload.Result = &result
	case JobStatusFailed:
		if job.Result != nil || !infrastructureCodePattern.MatchString(job.FailureCode) {
			return "", nil, nil, fmt.Errorf("failed webhook state is invalid")
		}
		payload.EventType = "judge.job.failed"
		payload.FailureCode = job.FailureCode
	case JobStatusCancelled:
		if job.Result != nil || job.FailureCode != "" {
			return "", nil, nil, fmt.Errorf("cancelled webhook state is invalid")
		}
		payload.EventType = "judge.job.cancelled"
	default:
		return "", nil, nil, fmt.Errorf("webhook job is not terminal")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil, nil, fmt.Errorf("encode terminal webhook payload: %w", err)
	}
	return payload.EventType, append([]byte(nil), encoded...), append([]byte(nil), encoded...), nil
}
