package external

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func (repository *MySQLJobRepository) insertTerminalWebhookEvent(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	job ExternalJobRecord,
) (string, error) {
	var tenantInternalID uint64
	var callbackInternalID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT tenant_id, callback_id FROM t_external_job WHERE id = ?`, job.InternalID).
		Scan(&tenantInternalID, &callbackInternalID); err != nil {
		return "", repositoryUnavailable("read terminal webhook ownership", err)
	}
	if !callbackInternalID.Valid {
		return "", nil
	}
	const maximumEventIDAttempts = 8
	for attempt := 0; attempt < maximumEventIDAttempts; attempt++ {
		eventID, err := generateExternalID(repository.random)
		if err != nil {
			return "", repositoryUnavailable("generate terminal webhook event ID", err)
		}
		eventType, payloadJSON, payloadBody, err := EncodeTerminalWebhookEvent(TerminalWebhookEvent{
			EventID: eventID, OccurredAt: now, Job: job,
		})
		if err != nil {
			return "", repositoryUnavailable("encode terminal webhook event", err)
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO t_external_webhook_outbox(
    event_id, tenant_id, job_id, callback_id, event_type,
    payload_json, payload_body, status, attempt_count, next_attempt_at, created_at, expires_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, 'PENDING', 0, ?, ?, ?)`,
			eventID, tenantInternalID, job.InternalID, callbackInternalID.Int64, eventType,
			payloadJSON, payloadBody, now, now, now.Add(repository.webhookDeliveryWindow))
		if err == nil {
			return eventID, nil
		}
		var mysqlError *mysqlDriver.MySQLError
		if !errors.As(err, &mysqlError) || mysqlError.Number != 1062 {
			return "", repositoryUnavailable("persist terminal webhook event", err)
		}

		var authoritativeEventID, authoritativeType string
		var authoritativeTenantID, authoritativeCallbackID uint64
		var authoritativeBody []byte
		var authoritativeOccurredAt time.Time
		err = tx.QueryRowContext(ctx, `
SELECT event_id, tenant_id, callback_id, event_type, payload_body, created_at
FROM t_external_webhook_outbox WHERE job_id = ? FOR UPDATE`, job.InternalID).Scan(
			&authoritativeEventID, &authoritativeTenantID, &authoritativeCallbackID,
			&authoritativeType, &authoritativeBody, &authoritativeOccurredAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			// The independent event-id key collided. Generate another ID while
			// preserving the same job transition and payload semantics.
			continue
		}
		if err != nil {
			return "", repositoryUnavailable("read authoritative terminal webhook event", err)
		}
		expectedType, _, expectedBody, encodeErr := EncodeTerminalWebhookEvent(TerminalWebhookEvent{
			EventID: authoritativeEventID, OccurredAt: authoritativeOccurredAt, Job: job,
		})
		if encodeErr != nil || authoritativeTenantID != tenantInternalID ||
			authoritativeCallbackID != uint64(callbackInternalID.Int64) || authoritativeType != expectedType ||
			!bytes.Equal(authoritativeBody, expectedBody) {
			return "", repositoryUnavailable("verify authoritative terminal webhook event", fmt.Errorf("conflicting event for terminal job"))
		}
		return authoritativeEventID, nil
	}
	return "", repositoryUnavailable("persist terminal webhook event", fmt.Errorf("event ID collision budget exhausted"))
}
