package external

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"time"
)

var ErrSourceRetentionNotAvailable = errors.New("source retention is not available")

type SourceRetentionClaim struct {
	TenantInternalID uint64
	JobInternalID    uint64
	SourceInternalID uint64
	JobExternalID    string
	SourceExternalID string
	ObjectKey        string
	DeleteToken      []byte
}

func (repository *MySQLJobRepository) ClaimSourceRetention(ctx context.Context, retention time.Duration) (SourceRetentionClaim, error) {
	if repository == nil || retention <= 0 || retention > 365*24*time.Hour {
		return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("begin source retention claim", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return SourceRetentionClaim{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM t_external_idempotency WHERE expires_at <= ?", now); err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("expire retained job idempotency", err)
	}
	var claim SourceRetentionClaim
	var markedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT job.tenant_id, job.id, source.id, job.external_id, source.external_id,
       source.object_key, source.delete_marked_at
FROM t_external_job AS job FORCE INDEX (idx_external_job_retention)
JOIN t_external_source_object AS source
  ON source.id = job.source_object_id AND source.tenant_id = job.tenant_id
WHERE job.status IN ('SUCCEEDED','FAILED','CANCELLED') AND job.completed_at <= ?
  AND source.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM t_external_webhook_outbox AS outbox WHERE outbox.job_id = job.id)
  AND NOT EXISTS (
      SELECT 1 FROM t_external_idempotency AS idem
      WHERE idem.tenant_id = job.tenant_id AND idem.resource_type = 'judge-job'
        AND idem.resource_external_id = job.external_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM t_external_job AS another
      WHERE another.source_object_id = source.id AND another.id <> job.id
  )
ORDER BY job.completed_at, job.id
LIMIT 1 FOR UPDATE SKIP LOCKED`, now.Add(-retention)).Scan(
		&claim.TenantInternalID, &claim.JobInternalID, &claim.SourceInternalID,
		&claim.JobExternalID, &claim.SourceExternalID, &claim.ObjectKey, &markedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceRetentionClaim{}, ErrSourceRetentionNotAvailable
	}
	if err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("lock retained source", err)
	}
	claim.DeleteToken = make([]byte, 32)
	if _, err := io.ReadFull(repository.random, claim.DeleteToken); err != nil {
		return SourceRetentionClaim{}, repositoryUnavailable("generate source retention fence", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_source_object
SET delete_marked_at = COALESCE(delete_marked_at, ?), delete_token = ?,
    delete_attempt_count = delete_attempt_count + 1, delete_last_error_code = NULL
WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
		now, claim.DeleteToken, claim.SourceInternalID, claim.TenantInternalID)
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

func (repository *MySQLJobRepository) RecordSourceRetentionFailure(ctx context.Context, claim SourceRetentionClaim) error {
	if repository == nil || !validSourceRetentionClaim(claim) {
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
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_source_object SET delete_last_error_code = 'OBJECT_DELETE_FAILED'
WHERE id = ? AND tenant_id = ? AND delete_token = ? AND deleted_at IS NULL`,
		claim.SourceInternalID, claim.TenantInternalID, claim.DeleteToken)
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
	var objectKey string
	if err := tx.QueryRowContext(ctx, `
SELECT object_key FROM t_external_source_object
WHERE id = ? AND tenant_id = ? AND delete_token = ? AND delete_marked_at IS NOT NULL
  AND deleted_at IS NULL FOR UPDATE`, claim.SourceInternalID, claim.TenantInternalID, claim.DeleteToken).Scan(&objectKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSourceRetentionNotAvailable
		}
		return repositoryUnavailable("lock source retention fence", err)
	}
	if objectKey != claim.ObjectKey {
		return ErrSourceRetentionNotAvailable
	}
	var status string
	if err := tx.QueryRowContext(ctx, `
SELECT status FROM t_external_job
WHERE id = ? AND tenant_id = ? AND source_object_id = ? FOR UPDATE`,
		claim.JobInternalID, claim.TenantInternalID, claim.SourceInternalID).Scan(&status); err != nil {
		return repositoryUnavailable("lock retained terminal job", err)
	}
	if status != string(JobStatusSucceeded) && status != string(JobStatusFailed) && status != string(JobStatusCancelled) {
		return ErrSourceRetentionNotAvailable
	}
	var blockers int
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?) +
       (SELECT COUNT(*) FROM t_external_idempotency
        WHERE tenant_id = ? AND resource_type = 'judge-job' AND resource_external_id = ?) +
       (SELECT COUNT(*) FROM t_external_job WHERE source_object_id = ? AND id <> ?)`,
		claim.JobInternalID, claim.TenantInternalID, claim.JobExternalID, claim.SourceInternalID, claim.JobInternalID).Scan(&blockers); err != nil {
		return repositoryUnavailable("recheck source retention references", err)
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
	if _, err := tx.ExecContext(ctx, "DELETE FROM t_external_job WHERE tenant_id = ? AND id = ?", claim.TenantInternalID, claim.JobInternalID); err != nil {
		return repositoryUnavailable("delete retained terminal job", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM t_external_source_object WHERE tenant_id = ? AND id = ? AND delete_token = ?", claim.TenantInternalID, claim.SourceInternalID, claim.DeleteToken); err != nil {
		return repositoryUnavailable("delete retained source metadata", err)
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit source retention finalize", err)
	}
	return nil
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
