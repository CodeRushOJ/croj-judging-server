package external

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

var infrastructureCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func (repository *MySQLJobRepository) LoadClaimSource(ctx context.Context, claim WorkerJobClaim) ([]byte, error) {
	if repository == nil || !validWorkerClaim(claim) {
		return nil, ErrInvalidJobState
	}
	var tenantExternalID string
	var source SourceObjectMetadata
	var keyVersion uint64
	err := repository.database.QueryRowContext(ctx, `
SELECT tenant.external_id, source.external_id, source.object_key, source.source_sha256,
       source.source_size_bytes, source.encryption_key_version, source.encryption_nonce
FROM t_external_job AS job
JOIN t_external_tenant AS tenant ON tenant.id = job.tenant_id
JOIN t_external_source_object AS source
  ON source.id = job.source_object_id AND source.tenant_id = job.tenant_id
WHERE job.id = ? AND job.status = 'RUNNING' AND job.attempt_no = ? AND job.worker_id = ?
  AND job.lease_token = ? AND job.lease_until > ?`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken, repository.now().UTC()).
		Scan(
			&tenantExternalID, &source.ExternalID, &source.ObjectKey, &source.SHA256,
			&source.SizeBytes, &keyVersion, &source.Nonce,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStaleJobClaim
	}
	if err != nil || keyVersion == 0 || keyVersion > 65535 {
		return nil, repositoryUnavailable("load authoritative source metadata", err)
	}
	source.KeyVersion = uint16(keyVersion)
	ciphertext, err := repository.sourceObjects.Get(ctx, source.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("%w: encrypted source object is unavailable", ErrSourceEncryption)
	}
	encrypted := EncryptedSource{
		Ciphertext: ciphertext,
		Nonce:      append([]byte(nil), source.Nonce...),
		KeyVersion: source.KeyVersion,
		SHA256:     append([]byte(nil), source.SHA256...),
		SizeBytes:  source.SizeBytes,
	}
	plaintext, err := repository.sourceCipher.Decrypt(
		tenantExternalID,
		source.ExternalID,
		encrypted,
	)
	clear(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: encrypted source authentication failed", ErrSourceEncryption)
	}
	return plaintext, nil
}

func (repository *MySQLJobRepository) ClaimNext(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WorkerJobClaim, error) {
	if repository == nil || !validWorkerID(workerID) || leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return WorkerJobClaim{}, ErrInvalidJobState
	}
	for recovered := 0; recovered < 16; recovered++ {
		claim, recoveryOnly, err := repository.claimOne(ctx, workerID, leaseDuration)
		if err != nil {
			return WorkerJobClaim{}, err
		}
		if !recoveryOnly {
			return claim, nil
		}
	}
	return WorkerJobClaim{}, ErrJobNotClaimable
}

func (repository *MySQLJobRepository) claimOne(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WorkerJobClaim, bool, error) {
	now := repository.now().UTC()
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("begin worker claim", err)
	}
	defer tx.Rollback()

	var candidateTenantID uint64
	err = tx.QueryRowContext(ctx, `
SELECT tenant_id
FROM t_external_job FORCE INDEX (idx_external_job_claim)
WHERE (status = 'QUEUED' AND next_attempt_at <= ?)
   OR (status = 'RUNNING' AND lease_until <= ?)
ORDER BY (status = 'RUNNING') DESC, next_attempt_at, created_at, id
LIMIT 1`, now, now).Scan(&candidateTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerJobClaim{}, false, ErrJobNotClaimable
	}
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("select claim tenant", err)
	}

	var tenantStatus string
	var encodedPolicy []byte
	if err := tx.QueryRowContext(ctx,
		"SELECT status, policy_json FROM t_external_tenant WHERE id = ? FOR UPDATE",
		candidateTenantID).Scan(&tenantStatus, &encodedPolicy); err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("lock claim tenant", err)
	}
	policy, err := decodeTenantPolicy(encodedPolicy)
	if err != nil || tenantStatus != "ACTIVE" {
		return WorkerJobClaim{}, false, ErrExternalJobUnavailable
	}

	var jobInternalID uint64
	var jobExternalID string
	var status JobStatus
	var attemptNo uint32
	var cancelRequested sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT id, external_id, status, attempt_no, cancel_requested_at
FROM t_external_job FORCE INDEX (idx_external_job_claim)
WHERE tenant_id = ? AND (
    (status = 'QUEUED' AND next_attempt_at <= ?) OR
    (status = 'RUNNING' AND lease_until <= ?)
)
ORDER BY (status = 'RUNNING') DESC, next_attempt_at, created_at, id
LIMIT 1 FOR UPDATE SKIP LOCKED`, candidateTenantID, now, now).
		Scan(&jobInternalID, &jobExternalID, &status, &attemptNo, &cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerJobClaim{}, false, ErrJobNotClaimable
	}
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("lock claimable job", err)
	}

	if status == JobStatusRunning {
		if err := expireAttempt(ctx, tx, candidateTenantID, jobInternalID, attemptNo, now); err != nil {
			return WorkerJobClaim{}, false, err
		}
		if cancelRequested.Valid {
			if _, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'CANCELLED', completed_at = ?, worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, now, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("recover expired cancellation", err)
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit expired cancellation", err)
			}
			return WorkerJobClaim{}, true, nil
		}
		if int(attemptNo) >= policy.MaxInfrastructureTries {
			if _, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'FAILED', failure_code = 'LEASE_EXPIRED', completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, now, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("finish exhausted expired job", err)
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit exhausted expired job", err)
			}
			return WorkerJobClaim{}, true, nil
		}
	} else if status == JobStatusQueued {
		var running int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM t_external_job WHERE tenant_id = ? AND status = 'RUNNING'",
			candidateTenantID).Scan(&running); err != nil {
			return WorkerJobClaim{}, false, repositoryUnavailable("establish running quota", err)
		}
		if running >= policy.MaxRunningJobs {
			return WorkerJobClaim{}, false, ErrJobNotClaimable
		}
	} else {
		return WorkerJobClaim{}, false, ErrJobNotClaimable
	}

	if attemptNo == ^uint32(0) {
		return WorkerJobClaim{}, false, ErrExternalJobUnavailable
	}
	newAttemptNo := attemptNo + 1
	leaseToken := make([]byte, 32)
	if _, err := io.ReadFull(repository.random, leaseToken); err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("generate lease token", err)
	}
	leaseUntil := now.Add(leaseDuration)
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'RUNNING', attempt_no = ?, worker_id = ?, lease_token = ?, lease_until = ?,
    started_at = COALESCE(started_at, ?), failure_code = NULL
WHERE id = ? AND status = ? AND attempt_no = ?`,
		newAttemptNo, workerID, leaseToken, leaseUntil, now,
		jobInternalID, status, attemptNo)
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("persist worker claim", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return WorkerJobClaim{}, false, ErrJobNotClaimable
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO t_external_job_attempt(
    tenant_id, job_id, attempt_no, worker_id, lease_token, status, lease_until, started_at
) VALUES (?, ?, ?, ?, ?, 'RUNNING', ?, ?)`,
		candidateTenantID, jobInternalID, newAttemptNo, workerID, leaseToken, leaseUntil, now); err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("persist worker attempt", err)
	}
	job, err := getExternalJobByInternalID(ctx, tx, jobInternalID)
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("read claimed job", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("commit worker claim", err)
	}
	return WorkerJobClaim{
		Job: job, WorkerID: workerID, AttemptNo: newAttemptNo,
		LeaseToken: append([]byte(nil), leaseToken...), LeaseUntil: leaseUntil,
	}, false, nil
}

func expireAttempt(ctx context.Context, tx *sql.Tx, tenantID, jobID uint64, attemptNo uint32, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_job_attempt
SET status = 'EXPIRED', finished_at = ?, failure_code = 'LEASE_EXPIRED'
WHERE tenant_id = ? AND job_id = ? AND attempt_no = ? AND status = 'RUNNING'`,
		now, tenantID, jobID, attemptNo)
	if err != nil {
		return repositoryUnavailable("expire worker attempt", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	return nil
}

func (repository *MySQLJobRepository) Heartbeat(
	ctx context.Context,
	claim WorkerJobClaim,
	leaseDuration time.Duration,
) error {
	if repository == nil || !validWorkerClaim(claim) || leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return ErrStaleJobClaim
	}
	now := repository.now().UTC()
	leaseUntil := now.Add(leaseDuration)
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin heartbeat", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET lease_until = ?
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > ?`,
		leaseUntil, claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken, now)
	if err != nil {
		return repositoryUnavailable("heartbeat job", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	result, err = tx.ExecContext(ctx, `
UPDATE t_external_job_attempt
SET lease_until = ?
WHERE job_id = ? AND attempt_no = ? AND worker_id = ? AND lease_token = ? AND status = 'RUNNING'`,
		leaseUntil, claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return repositoryUnavailable("heartbeat attempt", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit heartbeat", err)
	}
	return nil
}

func (repository *MySQLJobRepository) Complete(
	ctx context.Context,
	claim WorkerJobClaim,
	result DurableJobResult,
) error {
	if repository == nil || !validWorkerClaim(claim) || validateDurableJobResult(result) != nil {
		return ErrInvalidJobState
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return ErrInvalidJobState
	}
	now := repository.now().UTC()
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin completion", err)
	}
	defer tx.Rollback()
	cancelled, err := lockActiveClaim(ctx, tx, claim, now)
	if err != nil {
		return err
	}
	status := JobStatusSucceeded
	attemptStatus := "SUCCEEDED"
	var persistedResult any = resultJSON
	if cancelled {
		status = JobStatusCancelled
		attemptStatus = "CANCELLED"
		persistedResult = nil
	}
	jobResult, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = ?, result_json = ?, failure_code = NULL, completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ? AND lease_token = ?`,
		status, persistedResult, now, claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return repositoryUnavailable("complete job", err)
	}
	if affected, err := jobResult.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	if err := finishAttempt(ctx, tx, claim, attemptStatus, "", now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return repositoryUnavailable("commit completion", err)
	}
	return nil
}

func (repository *MySQLJobRepository) FailInfrastructure(
	ctx context.Context,
	claim WorkerJobClaim,
	failure InfrastructureFailure,
) (FailureDisposition, error) {
	if repository == nil || !validWorkerClaim(claim) || !infrastructureCodePattern.MatchString(failure.Code) || failure.RetryDelay < 0 || failure.RetryDelay > time.Hour {
		return "", ErrInvalidJobState
	}
	now := repository.now().UTC()
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", repositoryUnavailable("begin infrastructure failure", err)
	}
	defer tx.Rollback()
	cancelled, encodedPolicy, err := lockActiveClaimAndPolicy(ctx, tx, claim, now)
	if err != nil {
		return "", err
	}
	policy, err := decodeTenantPolicy(encodedPolicy)
	if err != nil {
		return "", ErrExternalJobUnavailable
	}
	disposition := FailureTerminal
	jobStatus := JobStatusFailed
	completedAt := any(now)
	nextAttemptAt := any(now)
	if cancelled {
		disposition = FailureCancelled
		jobStatus = JobStatusCancelled
	} else if int(claim.AttemptNo) < policy.MaxInfrastructureTries {
		disposition = FailureRequeued
		jobStatus = JobStatusQueued
		completedAt = nil
		nextAttemptAt = now.Add(failure.RetryDelay)
	}
	jobResult, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = ?, next_attempt_at = ?, failure_code = ?, completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ? AND lease_token = ?`,
		jobStatus, nextAttemptAt, failure.Code, completedAt,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return "", repositoryUnavailable("persist infrastructure failure", err)
	}
	if affected, err := jobResult.RowsAffected(); err != nil || affected != 1 {
		return "", ErrStaleJobClaim
	}
	attemptStatus := "FAILED"
	if cancelled {
		attemptStatus = "CANCELLED"
	}
	if err := finishAttempt(ctx, tx, claim, attemptStatus, failure.Code, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", repositoryUnavailable("commit infrastructure failure", err)
	}
	return disposition, nil
}

func lockActiveClaim(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim, now time.Time) (bool, error) {
	var cancelRequested sql.NullTime
	err := tx.QueryRowContext(ctx, `
SELECT cancel_requested_at FROM t_external_job
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > ? FOR UPDATE`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken, now).Scan(&cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrStaleJobClaim
	}
	if err != nil {
		return false, repositoryUnavailable("lock active claim", err)
	}
	return cancelRequested.Valid, nil
}

func lockActiveClaimAndPolicy(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim, now time.Time) (bool, []byte, error) {
	var cancelRequested sql.NullTime
	var encodedPolicy []byte
	err := tx.QueryRowContext(ctx, `
SELECT job.cancel_requested_at, tenant.policy_json
FROM t_external_job AS job
JOIN t_external_tenant AS tenant ON tenant.id = job.tenant_id
WHERE job.id = ? AND job.status = 'RUNNING' AND job.attempt_no = ? AND job.worker_id = ?
  AND job.lease_token = ? AND job.lease_until > ? FOR UPDATE`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken, now).
		Scan(&cancelRequested, &encodedPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrStaleJobClaim
	}
	if err != nil {
		return false, nil, repositoryUnavailable("lock active claim policy", err)
	}
	return cancelRequested.Valid, encodedPolicy, nil
}

func finishAttempt(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim, status, failureCode string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_job_attempt
SET status = ?, finished_at = ?, failure_code = NULLIF(?, '')
WHERE job_id = ? AND attempt_no = ? AND worker_id = ? AND lease_token = ? AND status = 'RUNNING'`,
		status, now, failureCode, claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return repositoryUnavailable("finish worker attempt", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	return nil
}

func getExternalJobByInternalID(ctx context.Context, queryer queryerSQL, jobInternalID uint64) (ExternalJobRecord, error) {
	return scanExternalJob(queryer.QueryRowContext(ctx, externalJobSelect+" WHERE job.id = ?", jobInternalID))
}

func decodeTenantPolicy(encoded []byte) (TenantPolicy, error) {
	var policy TenantPolicy
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return TenantPolicy{}, err
	}
	if err := policy.validate(); err != nil {
		return TenantPolicy{}, err
	}
	return policy, nil
}

func validWorkerID(workerID string) bool {
	if len(workerID) == 0 || len(workerID) > 128 || strings.TrimSpace(workerID) != workerID {
		return false
	}
	for index := 0; index < len(workerID); index++ {
		if workerID[index] < 0x21 || workerID[index] > 0x7e {
			return false
		}
	}
	return true
}

func validWorkerClaim(claim WorkerJobClaim) bool {
	return claim.Job.InternalID > 0 && claim.AttemptNo > 0 && validWorkerID(claim.WorkerID) && len(claim.LeaseToken) == 32
}
