package external

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrWebhookNotAvailable      = errors.New("webhook delivery is not available")
	ErrWebhookLeaseLost         = errors.New("webhook delivery lease is lost")
	ErrWebhookSettlementInvalid = errors.New("webhook settlement is invalid")
)

type MySQLWebhookOutboxRepositoryConfig struct {
	Database        *sql.DB
	Random          io.Reader
	MaximumAttempts uint
}

type MySQLWebhookOutboxRepository struct {
	database        *sql.DB
	random          io.Reader
	maximumAttempts uint
}

type WebhookClaim struct {
	OutboxID       uint64
	EventID        string
	EventType      string
	TenantID       string
	CallbackID     string
	DestinationURL string
	AllowedHost    string
	AllowedPort    uint16
	Body           []byte
	AttemptCount   uint
	WorkerID       string
	LeaseToken     []byte
	LeaseUntil     time.Time
	ExpiresAt      time.Time
	secret         EncryptedCallbackSecret
}

func (WebhookClaim) String() string   { return "[REDACTED WEBHOOK CLAIM]" }
func (WebhookClaim) GoString() string { return "[REDACTED WEBHOOK CLAIM]" }
func (WebhookClaim) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED WEBHOOK CLAIM]"`), nil
}

func (claim WebhookClaim) EncryptedSecret() EncryptedCallbackSecret {
	return EncryptedCallbackSecret{
		Ciphertext: append([]byte(nil), claim.secret.Ciphertext...),
		Nonce:      append([]byte(nil), claim.secret.Nonce...),
		KeyVersion: claim.secret.KeyVersion,
	}
}

type WebhookSettlement struct {
	Disposition WebhookDisposition
	HTTPStatus  int
	ErrorCode   string
	RetryAt     time.Time
}

func NewMySQLWebhookOutboxRepository(config MySQLWebhookOutboxRepositoryConfig) (*MySQLWebhookOutboxRepository, error) {
	if config.MaximumAttempts == 0 {
		config.MaximumAttempts = 12
	}
	if config.Database == nil || config.Random == nil || config.MaximumAttempts > 100 {
		return nil, fmt.Errorf("webhook database, random source, and maximum attempts from 1 to 100 are required")
	}
	return &MySQLWebhookOutboxRepository{
		database: config.Database, random: config.Random, maximumAttempts: config.MaximumAttempts,
	}, nil
}

func (repository *MySQLWebhookOutboxRepository) ClaimNextWebhook(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WebhookClaim, error) {
	if repository == nil || !validWorkerID(workerID) || leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return WebhookClaim{}, ErrWebhookSettlementInvalid
	}
	const maintenanceBudget = 64
	for maintained := 0; maintained < maintenanceBudget; maintained++ {
		claim, retry, err := repository.claimWebhook(ctx, workerID, leaseDuration)
		if err != nil {
			return WebhookClaim{}, err
		}
		if !retry {
			return claim, nil
		}
	}
	return WebhookClaim{}, ErrWebhookNotAvailable
}

func (repository *MySQLWebhookOutboxRepository) claimWebhook(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WebhookClaim, bool, error) {
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return WebhookClaim{}, false, repositoryUnavailable("begin webhook claim", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return WebhookClaim{}, false, err
	}

	var claim WebhookClaim
	var tenantStatus string
	var callbackDisabled sql.NullTime
	var allowedPort, secretVersion uint64
	err = tx.QueryRowContext(ctx, `
SELECT outbox.id, outbox.event_id, outbox.event_type, outbox.payload_body,
       outbox.attempt_count, outbox.expires_at,
       tenant.external_id, tenant.status,
       callback.external_id, callback.destination_url, callback.allowed_host,
       callback.allowed_port, callback.secret_ciphertext, callback.secret_nonce,
       callback.secret_key_version, callback.disabled_at
FROM t_external_webhook_outbox AS outbox FORCE INDEX (idx_external_webhook_delivery)
JOIN t_external_tenant AS tenant ON tenant.id = outbox.tenant_id
JOIN t_external_callback AS callback
  ON callback.id = outbox.callback_id AND callback.tenant_id = outbox.tenant_id
WHERE (
    (outbox.status = 'PENDING' AND outbox.next_attempt_at <= ?)
 OR (outbox.status = 'DELIVERING' AND outbox.lease_until <= ?)
)
AND NOT EXISTS (
    SELECT 1 FROM t_external_webhook_outbox AS earlier
    WHERE earlier.tenant_id = outbox.tenant_id
      AND (
          (earlier.status = 'PENDING' AND earlier.next_attempt_at <= ?)
       OR (earlier.status = 'DELIVERING' AND earlier.lease_until <= ?)
      )
      AND (
          CASE WHEN earlier.status = 'DELIVERING' THEN earlier.lease_until ELSE earlier.next_attempt_at END
              < CASE WHEN outbox.status = 'DELIVERING' THEN outbox.lease_until ELSE outbox.next_attempt_at END
          OR (
              CASE WHEN earlier.status = 'DELIVERING' THEN earlier.lease_until ELSE earlier.next_attempt_at END
                  = CASE WHEN outbox.status = 'DELIVERING' THEN outbox.lease_until ELSE outbox.next_attempt_at END
              AND earlier.id < outbox.id
          )
      )
)
ORDER BY CASE WHEN outbox.status = 'DELIVERING' THEN outbox.lease_until ELSE outbox.next_attempt_at END,
         outbox.tenant_id, outbox.id
LIMIT 1 FOR UPDATE SKIP LOCKED`, now, now, now, now).Scan(
		&claim.OutboxID, &claim.EventID, &claim.EventType, &claim.Body,
		&claim.AttemptCount, &claim.ExpiresAt,
		&claim.TenantID, &tenantStatus,
		&claim.CallbackID, &claim.DestinationURL, &claim.AllowedHost,
		&allowedPort, &claim.secret.Ciphertext, &claim.secret.Nonce,
		&secretVersion, &callbackDisabled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookClaim{}, false, ErrWebhookNotAvailable
	}
	if err != nil {
		return WebhookClaim{}, false, repositoryUnavailable("lock claimable webhook", err)
	}

	deadCode := ""
	if tenantStatus != "ACTIVE" {
		deadCode = "tenant_disabled"
	} else if callbackDisabled.Valid {
		deadCode = "callback_disabled"
	} else if !claim.ExpiresAt.After(now) {
		deadCode = "delivery_expired"
	} else if claim.AttemptCount >= repository.maximumAttempts {
		deadCode = "attempts_exhausted"
	} else if allowedPort == 0 || allowedPort > 65535 || secretVersion == 0 || secretVersion > 65535 ||
		len(claim.secret.Ciphertext) <= 16 || len(claim.secret.Nonce) != callbackSecretNonceBytes {
		deadCode = "callback_invalid"
	}
	if deadCode != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'DEAD', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    last_error_code = ?, dead_at = ?
WHERE id = ?`, deadCode, now, claim.OutboxID); err != nil {
			return WebhookClaim{}, false, repositoryUnavailable("dead-letter undeliverable webhook", err)
		}
		if err := tx.Commit(); err != nil {
			return WebhookClaim{}, false, repositoryUnavailable("commit undeliverable webhook", err)
		}
		return WebhookClaim{}, true, nil
	}

	token := make([]byte, 32)
	if _, err := io.ReadFull(repository.random, token); err != nil {
		return WebhookClaim{}, false, repositoryUnavailable("generate webhook lease token", err)
	}
	leaseUntil := now.Add(leaseDuration)
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'DELIVERING', attempt_count = attempt_count + 1,
    worker_id = ?, lease_token = ?, lease_until = ?
WHERE id = ?`, workerID, token, leaseUntil, claim.OutboxID)
	if err != nil {
		return WebhookClaim{}, false, repositoryUnavailable("lease webhook", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return WebhookClaim{}, false, ErrWebhookNotAvailable
	}
	if err := tx.Commit(); err != nil {
		return WebhookClaim{}, false, repositoryUnavailable("commit webhook claim", err)
	}
	claim.AllowedPort = uint16(allowedPort)
	claim.secret.KeyVersion = uint16(secretVersion)
	claim.AttemptCount++
	claim.WorkerID = workerID
	claim.LeaseToken = append([]byte(nil), token...)
	claim.LeaseUntil = leaseUntil
	claim.Body = append([]byte(nil), claim.Body...)
	claim.secret.Ciphertext = append([]byte(nil), claim.secret.Ciphertext...)
	claim.secret.Nonce = append([]byte(nil), claim.secret.Nonce...)
	return claim, false, nil
}

func (repository *MySQLWebhookOutboxRepository) SettleWebhook(
	ctx context.Context,
	claim WebhookClaim,
	settlement WebhookSettlement,
) error {
	if repository == nil || !validWebhookClaim(claim) || !validWebhookSettlement(settlement) {
		return ErrWebhookSettlementInvalid
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin webhook settlement", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return err
	}
	var expiresAt time.Time
	var attemptCount uint
	err = tx.QueryRowContext(ctx, `
SELECT expires_at, attempt_count
FROM t_external_webhook_outbox
WHERE id = ? AND status = 'DELIVERING' AND attempt_count = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > ?
FOR UPDATE`, claim.OutboxID, claim.AttemptCount, claim.WorkerID, claim.LeaseToken, now).
		Scan(&expiresAt, &attemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWebhookLeaseLost
	}
	if err != nil {
		return repositoryUnavailable("lock webhook settlement", err)
	}

	httpStatus := nullableWebhookHTTPStatus(settlement.HTTPStatus)
	switch settlement.Disposition {
	case WebhookDelivered:
		_, err = tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'DELIVERED', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    last_http_status = ?, last_error_code = NULL, delivered_at = ?
WHERE id = ?`, httpStatus, now, claim.OutboxID)
	case WebhookPermanentFailure:
		_, err = tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'DEAD', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    last_http_status = ?, last_error_code = ?, dead_at = ?
WHERE id = ?`, httpStatus, settlement.ErrorCode, now, claim.OutboxID)
	case WebhookRetry:
		if !settlement.RetryAt.After(now) || settlement.RetryAt.Sub(now) > maximumWebhookRetryAfter {
			return ErrWebhookSettlementInvalid
		}
		if attemptCount >= repository.maximumAttempts || !settlement.RetryAt.Before(expiresAt) {
			_, err = tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'DEAD', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    last_http_status = ?, last_error_code = ?, dead_at = ?
WHERE id = ?`, httpStatus, settlement.ErrorCode, now, claim.OutboxID)
		} else {
			_, err = tx.ExecContext(ctx, `
UPDATE t_external_webhook_outbox
SET status = 'PENDING', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    next_attempt_at = ?, last_http_status = ?, last_error_code = ?
WHERE id = ?`, settlement.RetryAt.UTC(), httpStatus, settlement.ErrorCode, claim.OutboxID)
		}
	}
	if err != nil {
		return repositoryUnavailable("persist webhook settlement", err)
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit webhook settlement", err)
	}
	return nil
}

func (repository *MySQLWebhookOutboxRepository) SweepTerminal(
	ctx context.Context,
	retention time.Duration,
	limit int,
) (int64, error) {
	if repository == nil || retention <= 0 || retention > 365*24*time.Hour || limit < 1 || limit > 1000 {
		return 0, ErrWebhookSettlementInvalid
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, repositoryUnavailable("begin webhook retention sweep", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id FROM t_external_webhook_outbox
WHERE (status = 'DELIVERED' AND delivered_at <= ?)
   OR (status = 'DEAD' AND dead_at <= ?)
ORDER BY COALESCE(delivered_at, dead_at), id
LIMIT ? FOR UPDATE SKIP LOCKED`, now.Add(-retention), now.Add(-retention), limit)
	if err != nil {
		return 0, repositoryUnavailable("lock terminal webhooks for retention", err)
	}
	ids := make([]uint64, 0, limit)
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, repositoryUnavailable("scan terminal webhook retention", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, repositoryUnavailable("close terminal webhook retention rows", err)
	}
	if err := rows.Err(); err != nil {
		return 0, repositoryUnavailable("iterate terminal webhook retention", err)
	}
	var deleted int64
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, "DELETE FROM t_external_webhook_outbox WHERE id = ?", id)
		if err != nil {
			return 0, repositoryUnavailable("delete terminal webhook", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, repositoryUnavailable("count deleted terminal webhook", err)
		}
		deleted += affected
	}
	if err := tx.Commit(); err != nil {
		return 0, repositoryUnavailable("commit webhook retention sweep", err)
	}
	return deleted, nil
}

func validWebhookClaim(claim WebhookClaim) bool {
	return claim.OutboxID > 0 && claim.AttemptCount > 0 && validWorkerID(claim.WorkerID) && len(claim.LeaseToken) == 32
}

func validWebhookSettlement(settlement WebhookSettlement) bool {
	statusValid := settlement.HTTPStatus == 0 || settlement.HTTPStatus >= 100 && settlement.HTTPStatus <= 599
	if !statusValid {
		return false
	}
	switch settlement.Disposition {
	case WebhookDelivered:
		return settlement.HTTPStatus >= 200 && settlement.HTTPStatus <= 299 && settlement.ErrorCode == "" && settlement.RetryAt.IsZero()
	case WebhookRetry:
		return !settlement.RetryAt.IsZero() && (settlement.ErrorCode == WebhookErrorNetwork && settlement.HTTPStatus == 0 ||
			settlement.ErrorCode == WebhookErrorHTTPRetryable && retryableWebhookHTTPStatus(settlement.HTTPStatus))
	case WebhookPermanentFailure:
		if !settlement.RetryAt.IsZero() {
			return false
		}
		if settlement.ErrorCode == WebhookErrorHTTPPermanent {
			return permanentWebhookHTTPStatus(settlement.HTTPStatus)
		}
		return settlement.HTTPStatus == 0 && (settlement.ErrorCode == WebhookErrorUnsafeDestination ||
			settlement.ErrorCode == WebhookErrorConfiguration ||
			settlement.ErrorCode == WebhookErrorInvalidDelivery ||
			settlement.ErrorCode == WebhookErrorCallbackDecrypt)
	default:
		return false
	}
}

func retryableWebhookHTTPStatus(status int) bool {
	return status == 408 || status == 425 || status == 429 || status >= 500 && status <= 599
}

func permanentWebhookHTTPStatus(status int) bool {
	return status >= 300 && status <= 499 && !retryableWebhookHTTPStatus(status)
}

func nullableWebhookHTTPStatus(status int) any {
	if status == 0 {
		return nil
	}
	return status
}
