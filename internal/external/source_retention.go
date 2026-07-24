package external

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

var (
	ErrSourceRetentionNotAvailable      = errors.New("source retention is not available")
	ErrIdempotencyRetentionNotAvailable = errors.New("idempotency retention is not available")
	errSourceRetentionContention        = errors.New("source retention claim contention")
)

// IsTransientDatabaseError identifies MySQL concurrency failures that are
// safe to retry at a worker-loop boundary. Callers must preserve the original
// driver error in the unwrap chain; generic availability/invariant failures
// intentionally fail closed instead of keeping an unhealthy Pod alive.
func IsTransientDatabaseError(err error) bool {
	var mysqlError *mysqlDriver.MySQLError
	return errors.As(err, &mysqlError) && (mysqlError.Number == 1205 || mysqlError.Number == 1213)
}

type SourceRetentionClaim struct {
	TenantInternalID uint64
	JobInternalID    uint64
	SourceInternalID uint64
	JobExternalID    string
	SourceExternalID string
	ObjectKey        string
	DeleteToken      []byte
}

func (repository *MySQLJobRepository) ExpireIdempotencyBatch(ctx context.Context, batch int) (int64, error) {
	if repository == nil || batch < 1 || batch > 1000 {
		return 0, ErrIdempotencyRetentionNotAvailable
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, repositoryUnavailable("begin idempotency retention batch", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT id FROM t_external_idempotency FORCE INDEX (idx_external_idempotency_expiry)
WHERE expires_at <= CURRENT_TIMESTAMP(3)
ORDER BY expires_at, id
LIMIT ? FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, repositoryUnavailable("lock idempotency retention batch", err)
	}
	ids := make([]uint64, 0, batch)
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, repositoryUnavailable("scan idempotency retention batch", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, repositoryUnavailable("iterate idempotency retention batch", err)
	}
	if err := rows.Close(); err != nil {
		return 0, repositoryUnavailable("close idempotency retention batch", err)
	}
	if len(ids) == 0 {
		return 0, ErrIdempotencyRetentionNotAvailable
	}
	arguments := make([]any, len(ids))
	placeholders := make([]string, len(ids))
	for index, id := range ids {
		arguments[index] = id
		placeholders[index] = "?"
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM t_external_idempotency WHERE id IN ("+strings.Join(placeholders, ",")+")", arguments...)
	if err != nil {
		return 0, repositoryUnavailable("expire idempotency batch", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, repositoryUnavailable("count expired idempotency batch", err)
	}
	if affected != int64(len(ids)) {
		return 0, repositoryUnavailable("expire idempotency batch", ErrExternalJobUnavailable)
	}
	if err := tx.Commit(); err != nil {
		return 0, repositoryUnavailable("commit idempotency retention batch", err)
	}
	return affected, nil
}

func (repository *MySQLJobRepository) ClaimSourceRetention(ctx context.Context, retention, leaseDuration time.Duration) (SourceRetentionClaim, error) {
	if repository == nil || retention <= 0 || retention > 365*24*time.Hour || leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
	}
	candidates, err := repository.sourceRetentionCandidates(ctx, retention, 32)
	if err != nil {
		return SourceRetentionClaim{}, err
	}
	for _, candidate := range candidates {
		claim, err := repository.claimSourceRetentionCandidate(ctx, candidate, retention, leaseDuration)
		if errors.Is(err, errSourceRetentionContention) || errors.Is(err, ErrSourceRetentionNotAvailable) {
			continue
		}
		return claim, err
	}
	return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
}

func (repository *MySQLJobRepository) sourceRetentionCandidates(ctx context.Context, retention time.Duration, limit int) ([]SourceRetentionClaim, error) {
	rows, err := repository.database.QueryContext(ctx, `
SELECT tenant_id, job_id, source_id, job_external_id, source_external_id, object_key
FROM (
    SELECT job.tenant_id, job.id AS job_id, source.id AS source_id,
           job.external_id AS job_external_id, source.external_id AS source_external_id,
           source.object_key, job.completed_at,
           ROW_NUMBER() OVER (PARTITION BY job.tenant_id ORDER BY job.completed_at, job.id) AS tenant_rank
    FROM t_external_job AS job FORCE INDEX (idx_external_job_retention)
    JOIN t_external_source_object AS source
      ON source.id = job.source_object_id AND source.tenant_id = job.tenant_id
    WHERE job.status IN ('SUCCEEDED','FAILED','CANCELLED')
      AND job.completed_at <= CURRENT_TIMESTAMP(3) - INTERVAL ? MICROSECOND
      AND source.deleted_at IS NULL
      AND (source.delete_marked_at IS NULL OR
           (source.delete_next_attempt_at <= CURRENT_TIMESTAMP(3) AND source.delete_lease_until <= CURRENT_TIMESTAMP(3)))
      AND NOT EXISTS (SELECT 1 FROM t_external_webhook_outbox AS outbox WHERE outbox.job_id = job.id)
      AND NOT EXISTS (
          SELECT 1 FROM t_external_idempotency AS idem
          WHERE idem.tenant_id = job.tenant_id AND idem.resource_type = 'judge-job'
            AND idem.resource_external_id = job.external_id
            AND idem.expires_at > CURRENT_TIMESTAMP(3)
      )
      AND NOT EXISTS (
          SELECT 1 FROM t_external_job AS another
          WHERE another.source_object_id = source.id AND another.id <> job.id
      )
) AS candidate
WHERE tenant_rank = 1
ORDER BY completed_at, job_id
LIMIT ?`, retention.Microseconds(), limit)
	if err != nil {
		return nil, repositoryUnavailable("select retained source candidates", err)
	}
	defer rows.Close()
	candidates := make([]SourceRetentionClaim, 0, limit)
	for rows.Next() {
		var claim SourceRetentionClaim
		if err := rows.Scan(
			&claim.TenantInternalID, &claim.JobInternalID, &claim.SourceInternalID,
			&claim.JobExternalID, &claim.SourceExternalID, &claim.ObjectKey,
		); err != nil {
			return nil, repositoryUnavailable("scan retained source candidate", err)
		}
		candidates = append(candidates, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, repositoryUnavailable("iterate retained source candidates", err)
	}
	return candidates, nil
}

func (repository *MySQLJobRepository) claimSourceRetentionCandidate(
	ctx context.Context,
	claim SourceRetentionClaim,
	retention, leaseDuration time.Duration,
) (SourceRetentionClaim, error) {
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("begin source retention claim", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return SourceRetentionClaim{}, err
	}
	if err := lockRetentionTenantJobForClaim(ctx, tx, claim, now.Add(-retention)); err != nil {
		return SourceRetentionClaim{}, err
	}
	var markedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT external_id, object_key, delete_marked_at
FROM t_external_source_object
WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL
  AND (delete_marked_at IS NULL OR (delete_next_attempt_at <= ? AND delete_lease_until <= ?))
FOR UPDATE SKIP LOCKED`, claim.SourceInternalID, claim.TenantInternalID, now, now).
		Scan(&claim.SourceExternalID, &claim.ObjectKey, &markedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceRetentionClaim{}, errSourceRetentionContention
	}
	if err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("lock retained source", err)
	}
	blockers, err := retentionBlockers(ctx, tx, claim)
	if err != nil {
		return SourceRetentionClaim{}, err
	}
	if blockers != 0 {
		return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
	}
	claim.DeleteToken = make([]byte, 32)
	if _, err := io.ReadFull(repository.random, claim.DeleteToken); err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("generate source retention fence", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_source_object
SET delete_marked_at = COALESCE(delete_marked_at, ?), delete_token = ?,
    delete_lease_until = ?, delete_next_attempt_at = ?,
    delete_attempt_count = delete_attempt_count + 1, delete_last_error_code = NULL
WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
		now, claim.DeleteToken, now.Add(leaseDuration), now,
		claim.SourceInternalID, claim.TenantInternalID)
	if err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("mark retained source", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
	}
	if !markedAt.Valid {
		if err := insertRetentionAudit(ctx, tx, claim, "MARKED", now); err != nil {
			return SourceRetentionClaim{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("commit source retention claim", err)
	}
	return claim, nil
}

// lockRetentionTenantJobForClaim keeps the global tenant -> job -> source
// mutation order while making multi-replica contention non-blocking. The
// source row is deliberately locked by the caller only after this succeeds.
func lockRetentionTenantJobForClaim(ctx context.Context, tx *sql.Tx, claim SourceRetentionClaim, completedBefore time.Time) error {
	var tenantID uint64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM t_external_tenant WHERE id = ? FOR UPDATE SKIP LOCKED", claim.TenantInternalID).Scan(&tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errSourceRetentionContention
		}
		return repositoryUnavailable("lock source retention tenant claim", err)
	}
	var jobExternalID, status string
	var sourceID uint64
	var completedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
SELECT external_id, status, source_object_id, completed_at
FROM t_external_job
WHERE id = ? AND tenant_id = ? FOR UPDATE SKIP LOCKED`, claim.JobInternalID, claim.TenantInternalID).
		Scan(&jobExternalID, &status, &sourceID, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errSourceRetentionContention
		}
		return repositoryUnavailable("lock retained terminal job claim", err)
	}
	terminal := status == string(JobStatusSucceeded) || status == string(JobStatusFailed) || status == string(JobStatusCancelled)
	if !terminal || sourceID != claim.SourceInternalID || jobExternalID != claim.JobExternalID || !completedAt.Valid || completedAt.Time.After(completedBefore) {
		return ErrSourceRetentionNotAvailable
	}
	return nil
}

func (repository *MySQLJobRepository) RecordSourceRetentionFailure(ctx context.Context, claim SourceRetentionClaim, retryDelay time.Duration) error {
	if repository == nil || !validSourceRetentionClaim(claim) || retryDelay <= 0 || retryDelay > time.Hour {
		return ErrSourceRetentionNotAvailable
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin source retention retry", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return err
	}
	if err := lockRetentionTenantJob(ctx, tx, claim, time.Time{}); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_source_object
SET delete_last_error_code = 'OBJECT_DELETE_FAILED', delete_lease_until = ?, delete_next_attempt_at = ?
WHERE id = ? AND tenant_id = ? AND delete_token = ? AND delete_lease_until > ? AND deleted_at IS NULL`,
		now, now.Add(retryDelay), claim.SourceInternalID, claim.TenantInternalID, claim.DeleteToken, now)
	if err != nil {
		return repositoryUnavailable("record source retention retry", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrSourceRetentionNotAvailable
	}
	if err := insertRetentionAudit(ctx, tx, claim, "DELETE_RETRY", now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit source retention retry", err)
	}
	return nil
}

func (repository *MySQLJobRepository) FinalizeSourceRetention(ctx context.Context, claim SourceRetentionClaim) error {
	if repository == nil || !validSourceRetentionClaim(claim) {
		return ErrSourceRetentionNotAvailable
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin source retention finalize", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return err
	}
	if err := lockRetentionTenantJob(ctx, tx, claim, time.Time{}); err != nil {
		return err
	}
	var objectKey string
	if err := tx.QueryRowContext(ctx, `
SELECT object_key FROM t_external_source_object
WHERE id = ? AND tenant_id = ? AND delete_token = ? AND delete_marked_at IS NOT NULL
  AND delete_lease_until > ? AND deleted_at IS NULL FOR UPDATE`,
		claim.SourceInternalID, claim.TenantInternalID, claim.DeleteToken, now).Scan(&objectKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceRetentionNotAvailable
		}
		return repositoryUnavailable("lock source retention fence", err)
	}
	if objectKey != claim.ObjectKey {
		return ErrSourceRetentionNotAvailable
	}
	blockers, err := retentionBlockers(ctx, tx, claim)
	if err != nil {
		return err
	}
	if blockers != 0 {
		return ErrSourceRetentionNotAvailable
	}
	if err := insertRetentionAudit(ctx, tx, claim, "DELETED", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM t_external_job_attempt WHERE tenant_id = ? AND job_id = ?", claim.TenantInternalID, claim.JobInternalID); err != nil {
		return repositoryUnavailable("delete retained attempts", err)
	}
	if result, err := tx.ExecContext(ctx, "DELETE FROM t_external_job WHERE tenant_id = ? AND id = ?", claim.TenantInternalID, claim.JobInternalID); err != nil {
		return repositoryUnavailable("delete retained terminal job", err)
	} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrSourceRetentionNotAvailable
	}
	if result, err := tx.ExecContext(ctx, "DELETE FROM t_external_source_object WHERE tenant_id = ? AND id = ? AND delete_token = ?", claim.TenantInternalID, claim.SourceInternalID, claim.DeleteToken); err != nil {
		return repositoryUnavailable("delete retained source metadata", err)
	} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrSourceRetentionNotAvailable
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit source retention finalize", err)
	}
	return nil
}

// lockRetentionTenantJob establishes the global mutation order tenant -> job.
// Source rows are locked only after this helper returns.
func lockRetentionTenantJob(ctx context.Context, tx *sql.Tx, claim SourceRetentionClaim, completedBefore time.Time) error {
	var tenantID uint64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM t_external_tenant WHERE id = ? FOR UPDATE", claim.TenantInternalID).Scan(&tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceRetentionNotAvailable
		}
		return repositoryUnavailable("lock source retention tenant", err)
	}
	var jobExternalID, status string
	var sourceID uint64
	var completedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
SELECT external_id, status, source_object_id, completed_at
FROM t_external_job
WHERE id = ? AND tenant_id = ? FOR UPDATE`, claim.JobInternalID, claim.TenantInternalID).
		Scan(&jobExternalID, &status, &sourceID, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceRetentionNotAvailable
		}
		return repositoryUnavailable("lock retained terminal job", err)
	}
	terminal := status == string(JobStatusSucceeded) || status == string(JobStatusFailed) || status == string(JobStatusCancelled)
	if !terminal || sourceID != claim.SourceInternalID || jobExternalID != claim.JobExternalID || !completedAt.Valid ||
		(!completedBefore.IsZero() && completedAt.Time.After(completedBefore)) {
		return ErrSourceRetentionNotAvailable
	}
	return nil
}

func retentionBlockers(ctx context.Context, tx *sql.Tx, claim SourceRetentionClaim) (int, error) {
	var blockers int
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?) +
       (SELECT COUNT(*) FROM t_external_idempotency
        WHERE tenant_id = ? AND resource_type = 'judge-job' AND resource_external_id = ?
          AND expires_at > CURRENT_TIMESTAMP(3)) +
       (SELECT COUNT(*) FROM t_external_job WHERE source_object_id = ? AND id <> ?)`,
		claim.JobInternalID, claim.TenantInternalID, claim.JobExternalID,
		claim.SourceInternalID, claim.JobInternalID).Scan(&blockers); err != nil {
		return 0, repositoryUnavailable("recheck source retention references", err)
	}
	return blockers, nil
}

func insertRetentionAudit(ctx context.Context, tx *sql.Tx, claim SourceRetentionClaim, event string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO t_external_retention_audit(
    tenant_id, job_external_id, source_external_id, event_type, event_at
) VALUES (?, ?, ?, ?, ?)`, claim.TenantInternalID, claim.JobExternalID, claim.SourceExternalID, event, now); err != nil {
		return repositoryUnavailable("append source retention audit", err)
	}
	return nil
}

func validSourceRetentionClaim(claim SourceRetentionClaim) bool {
	return claim.TenantInternalID > 0 && claim.JobInternalID > 0 && claim.SourceInternalID > 0 &&
		externalIDPattern.MatchString(claim.JobExternalID) && externalIDPattern.MatchString(claim.SourceExternalID) &&
		validSourceObjectKey(claim.ObjectKey) && len(claim.DeleteToken) == 32
}
