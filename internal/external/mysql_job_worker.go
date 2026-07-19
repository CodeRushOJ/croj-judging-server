package external

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

var infrastructureCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

type dailyReservationDecision uint8

const (
	dailyReservationAllowed dailyReservationDecision = iota + 1
	dailyReservationDeferred
	dailyReservationImpossible
)

func (repository *MySQLJobRepository) LoadClaimSource(ctx context.Context, claim WorkerJobClaim) ([]byte, error) {
	input, err := repository.LoadClaimInput(ctx, claim)
	if err != nil {
		return nil, err
	}
	return input.SourceCode, nil
}

func (repository *MySQLJobRepository) LoadClaimInput(ctx context.Context, claim WorkerJobClaim) (WorkerExecutionInput, error) {
	if repository == nil || !validWorkerClaim(claim) {
		return WorkerExecutionInput{}, ErrInvalidJobState
	}
	var tenantExternalID string
	var source SourceObjectMetadata
	var keyVersion uint64
	var input WorkerExecutionInput
	var bundleDigest []byte
	var manifestJSON []byte
	var encodedPolicy []byte
	err := repository.database.QueryRowContext(ctx, `
SELECT tenant.external_id, source.external_id, source.object_key, source.source_sha256,
       source.source_size_bytes, source.encryption_key_version, source.encryption_nonce,
       job.language_id, job.stop_on_failure, bundle.object_key, bundle.sha256,
       bundle.size_bytes, bundle.manifest_json, tenant.policy_json
FROM t_external_job AS job
JOIN t_external_tenant AS tenant ON tenant.id = job.tenant_id
JOIN t_external_source_object AS source
  ON source.id = job.source_object_id AND source.tenant_id = job.tenant_id
JOIN t_external_bundle AS bundle
  ON bundle.id = job.bundle_id AND bundle.tenant_id = job.tenant_id
WHERE job.id = ? AND job.status = 'RUNNING' AND job.attempt_no = ? AND job.worker_id = ?
  AND job.lease_token = ? AND job.lease_until > CURRENT_TIMESTAMP(3)
  AND tenant.status = 'ACTIVE' AND bundle.publication_status = 'READY'
  AND bundle.ready_at IS NOT NULL AND bundle.delete_marked_at IS NULL AND bundle.deleted_at IS NULL`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken).
		Scan(
			&tenantExternalID, &source.ExternalID, &source.ObjectKey, &source.SHA256,
			&source.SizeBytes, &keyVersion, &source.Nonce, &input.Language, &input.StopOnFailure,
			&input.Bundle.ObjectKey, &bundleDigest, &input.Bundle.SizeBytes, &manifestJSON, &encodedPolicy,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerExecutionInput{}, ErrStaleJobClaim
	}
	if err != nil || keyVersion == 0 || keyVersion > 65535 {
		return WorkerExecutionInput{}, repositoryUnavailable("load authoritative execution metadata", err)
	}
	manifest, err := bundle.ParseManifest(manifestJSON)
	if err != nil {
		return WorkerExecutionInput{}, repositoryUnavailable("validate authoritative bundle manifest", err)
	}
	policy, err := decodeTenantPolicy(encodedPolicy)
	if err != nil {
		return WorkerExecutionInput{}, repositoryUnavailable("enforce authoritative bundle limits", err)
	}
	if manifest.Limits.TimeLimitMillis > policy.MaxTimeLimitMillis || manifest.Limits.MemoryLimitMiB > policy.MaxMemoryLimitMiB {
		return WorkerExecutionInput{}, repositoryUnavailable("enforce authoritative bundle limits", ErrExternalJobInvalid)
	}
	if len(bundleDigest) != 32 {
		return WorkerExecutionInput{}, repositoryUnavailable("validate authoritative bundle digest", ErrExternalJobUnavailable)
	}
	input.Bundle.SHA256 = hex.EncodeToString(bundleDigest)
	input.Bundle.ManifestJSON = append([]byte(nil), manifestJSON...)
	input.Bundle.Manifest = manifest
	source.KeyVersion = uint16(keyVersion)
	ciphertext, err := repository.sourceObjects.Get(ctx, source.ObjectKey, source.SizeBytes+sourceCiphertextOverheadBytes)
	if err != nil {
		return WorkerExecutionInput{}, fmt.Errorf("%w: encrypted source object is unavailable", ErrSourceEncryption)
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
		return WorkerExecutionInput{}, fmt.Errorf("%w: encrypted source authentication failed", ErrSourceEncryption)
	}
	input.SourceCode = plaintext
	return input, nil
}

func (repository *MySQLJobRepository) ClaimNext(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WorkerJobClaim, error) {
	if repository == nil || !validWorkerID(workerID) || leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return WorkerJobClaim{}, ErrInvalidJobState
	}
	const maximumClaimRetries = 32
	for attempt := 0; attempt < maximumClaimRetries; attempt++ {
		claim, retry, err := repository.claimOne(ctx, workerID, leaseDuration)
		if err != nil {
			if retry && errors.Is(err, ErrJobNotClaimable) {
				if err := waitForClaimRetry(ctx); err != nil {
					return WorkerJobClaim{}, repositoryUnavailable("wait for worker claim contention", err)
				}
				continue
			}
			return WorkerJobClaim{}, err
		}
		if !retry {
			return claim, nil
		}
		if err := waitForClaimRetry(ctx); err != nil {
			return WorkerJobClaim{}, repositoryUnavailable("wait for worker claim recovery", err)
		}
	}
	return WorkerJobClaim{}, ErrJobNotClaimable
}

func (repository *MySQLJobRepository) claimOne(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (WorkerJobClaim, bool, error) {
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("begin worker claim", err)
	}
	defer tx.Rollback()
	leaseNow, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return WorkerJobClaim{}, false, err
	}

	var candidateTenantID uint64
	err = tx.QueryRowContext(ctx, `
	SELECT tenant.id
	FROM t_external_tenant AS tenant FORCE INDEX (idx_external_tenant_fair_claim)
	WHERE EXISTS (
	       SELECT 1 FROM t_external_job AS expired_job FORCE INDEX (idx_external_job_claim)
	       WHERE expired_job.tenant_id = tenant.id AND expired_job.status = 'RUNNING'
	         AND expired_job.lease_until <= ?
	   ) OR (tenant.status = 'ACTIVE'
	       AND JSON_TYPE(JSON_EXTRACT(tenant.policy_json, '$.maxInfrastructureTries')) = 'INTEGER'
	       AND CAST(JSON_UNQUOTE(JSON_EXTRACT(tenant.policy_json, '$.maxInfrastructureTries')) AS UNSIGNED) BETWEEN 1 AND 10
	       AND JSON_TYPE(JSON_EXTRACT(tenant.policy_json, '$.maxRunningJobs')) = 'INTEGER'
	       AND CAST(JSON_UNQUOTE(JSON_EXTRACT(tenant.policy_json, '$.maxRunningJobs')) AS UNSIGNED) > (
	           SELECT COUNT(*) FROM t_external_job AS running_job
	           WHERE running_job.tenant_id = tenant.id AND running_job.status = 'RUNNING'
	       )
	       AND EXISTS (
	           SELECT 1 FROM t_external_job AS queued_job FORCE INDEX (idx_external_job_claim)
	           WHERE queued_job.tenant_id = tenant.id AND queued_job.status = 'QUEUED'
	             AND queued_job.next_attempt_at <= ?
	       )
	   )
	ORDER BY EXISTS (
	    SELECT 1 FROM t_external_job AS recovery_job FORCE INDEX (idx_external_job_claim)
	    WHERE recovery_job.tenant_id = tenant.id AND recovery_job.status = 'RUNNING'
	      AND recovery_job.lease_until <= ?
	) DESC, tenant.last_claimed_at IS NULL DESC, tenant.last_claimed_at, tenant.id
	LIMIT 1 FOR UPDATE SKIP LOCKED`, leaseNow, leaseNow, leaseNow).Scan(&candidateTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		var due bool
		if err := tx.QueryRowContext(ctx, `
	SELECT EXISTS (
	    SELECT 1 FROM t_external_tenant AS tenant
	    WHERE EXISTS (
	        SELECT 1 FROM t_external_job AS expired_job
	        WHERE expired_job.tenant_id = tenant.id AND expired_job.status = 'RUNNING'
	          AND expired_job.lease_until <= ?
	    ) OR (tenant.status = 'ACTIVE'
	        AND JSON_TYPE(JSON_EXTRACT(tenant.policy_json, '$.maxRunningJobs')) = 'INTEGER'
	        AND CAST(JSON_UNQUOTE(JSON_EXTRACT(tenant.policy_json, '$.maxRunningJobs')) AS UNSIGNED) > (
	            SELECT COUNT(*) FROM t_external_job AS running_job
	            WHERE running_job.tenant_id = tenant.id AND running_job.status = 'RUNNING'
	        )
	        AND EXISTS (
	            SELECT 1 FROM t_external_job AS queued_job
	            WHERE queued_job.tenant_id = tenant.id AND queued_job.status = 'QUEUED'
	              AND queued_job.next_attempt_at <= ?
	        )
	    )
	)`, leaseNow, leaseNow).Scan(&due); err != nil {
			return WorkerJobClaim{}, false, repositoryUnavailable("check fair claim contention", err)
		}
		if due {
			return WorkerJobClaim{}, true, ErrJobNotClaimable
		}
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
	var policy TenantPolicy
	if tenantStatus == "ACTIVE" {
		policy, err = decodeTenantPolicy(encodedPolicy)
		if err != nil {
			return WorkerJobClaim{}, false, ErrExternalJobUnavailable
		}
	}

	var jobInternalID uint64
	var jobExternalID string
	var status JobStatus
	var attemptNo uint32
	var cancelRequested sql.NullTime
	var reservedExecutionMillis int64
	err = tx.QueryRowContext(ctx, `
	SELECT job.id, job.external_id, job.status, job.attempt_no, job.cancel_requested_at,
	       COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(bundle.manifest_json, '$.limits.timeLimitMillis')) AS UNSIGNED) * bundle.case_count, ?)
	FROM t_external_job AS job FORCE INDEX (idx_external_job_claim)
	JOIN t_external_bundle AS bundle ON bundle.id = job.bundle_id AND bundle.tenant_id = job.tenant_id
	WHERE job.tenant_id = ? AND (
	    (? = 'ACTIVE' AND job.status = 'QUEUED' AND job.next_attempt_at <= ?) OR
	    (job.status = 'RUNNING' AND job.lease_until <= ?)
	)
	ORDER BY (job.status = 'RUNNING') DESC, job.next_attempt_at, job.created_at, job.id
	LIMIT 1 FOR UPDATE SKIP LOCKED`, policy.MaxTimeLimitMillis, candidateTenantID, tenantStatus, leaseNow, leaseNow).
		Scan(&jobInternalID, &jobExternalID, &status, &attemptNo, &cancelRequested, &reservedExecutionMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerJobClaim{}, true, ErrJobNotClaimable
	}
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("lock claimable job", err)
	}

	if status == JobStatusRunning {
		if err := expireAttempt(ctx, tx, candidateTenantID, jobInternalID, attemptNo, leaseNow); err != nil {
			return WorkerJobClaim{}, false, err
		}
		if tenantStatus != "ACTIVE" {
			if _, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'FAILED', failure_code = 'TENANT_DISABLED', completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, leaseNow, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("settle disabled tenant job", err)
			}
			job, err := getExternalJobByInternalID(ctx, tx, jobInternalID)
			if err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("read disabled tenant job", err)
			}
			if _, err := repository.insertTerminalWebhookEvent(ctx, tx, leaseNow, job); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit disabled tenant job", err)
			}
			return WorkerJobClaim{}, true, nil
		}
		if cancelRequested.Valid {
			if _, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'CANCELLED', result_json = NULL, failure_code = NULL, completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, leaseNow, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("recover expired cancellation", err)
			}
			job, err := getExternalJobByInternalID(ctx, tx, jobInternalID)
			if err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("read recovered cancellation", err)
			}
			if _, err := repository.insertTerminalWebhookEvent(ctx, tx, leaseNow, job); err != nil {
				return WorkerJobClaim{}, false, err
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
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, leaseNow, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("finish exhausted expired job", err)
			}
			job, err := getExternalJobByInternalID(ctx, tx, jobInternalID)
			if err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("read exhausted expired job", err)
			}
			if _, err := repository.insertTerminalWebhookEvent(ctx, tx, leaseNow, job); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit exhausted expired job", err)
			}
			return WorkerJobClaim{}, true, nil
		}
		decision, err := reserveDailyExecution(ctx, tx, candidateTenantID, policy.DailyExecutionMillis, reservedExecutionMillis)
		if err != nil {
			return WorkerJobClaim{}, false, err
		}
		if decision == dailyReservationImpossible {
			if err := repository.failImpossibleDailyExecution(ctx, tx, candidateTenantID, jobInternalID, status, attemptNo, leaseNow); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit recovered impossible daily quota job", err)
			}
			return WorkerJobClaim{}, true, nil
		}
		if decision == dailyReservationDeferred {
			if _, err := tx.ExecContext(ctx, `
	UPDATE t_external_job
	SET status = 'QUEUED', worker_id = NULL, lease_token = NULL, lease_until = NULL,
	    next_attempt_at = TIMESTAMP(DATE_ADD(CURRENT_DATE, INTERVAL 1 DAY))
	WHERE id = ? AND status = 'RUNNING' AND attempt_no = ?`, jobInternalID, attemptNo); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("defer recovered daily quota job", err)
			}
			if err := advanceTenantFairness(ctx, tx, candidateTenantID, leaseNow); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit recovered daily quota deferral", err)
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
			return WorkerJobClaim{}, true, ErrJobNotClaimable
		}
		if reservedExecutionMillis <= 0 {
			return WorkerJobClaim{}, false, ErrExternalJobUnavailable
		}
		decision, err := reserveDailyExecution(ctx, tx, candidateTenantID, policy.DailyExecutionMillis, reservedExecutionMillis)
		if err != nil {
			return WorkerJobClaim{}, false, err
		}
		if decision == dailyReservationImpossible {
			if err := repository.failImpossibleDailyExecution(ctx, tx, candidateTenantID, jobInternalID, status, attemptNo, leaseNow); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit impossible daily quota job", err)
			}
			return WorkerJobClaim{}, true, nil
		}
		if decision == dailyReservationDeferred {
			if _, err := tx.ExecContext(ctx, `
	UPDATE t_external_job SET next_attempt_at = TIMESTAMP(DATE_ADD(CURRENT_DATE, INTERVAL 1 DAY))
	WHERE id = ? AND status = 'QUEUED'`, jobInternalID); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("defer daily quota job", err)
			}
			if err := advanceTenantFairness(ctx, tx, candidateTenantID, leaseNow); err != nil {
				return WorkerJobClaim{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return WorkerJobClaim{}, false, repositoryUnavailable("commit daily quota deferral", err)
			}
			return WorkerJobClaim{}, true, nil
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
	leaseUntil := leaseNow.Add(leaseDuration)
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'RUNNING', attempt_no = ?, worker_id = ?, lease_token = ?, lease_until = ?,
    started_at = COALESCE(started_at, ?), failure_code = NULL
WHERE id = ? AND status = ? AND attempt_no = ?`,
		newAttemptNo, workerID, leaseToken, leaseUntil, leaseNow,
		jobInternalID, status, attemptNo)
	if err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("persist worker claim", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return WorkerJobClaim{}, false, ErrJobNotClaimable
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO t_external_job_attempt(
	    tenant_id, job_id, attempt_no, worker_id, lease_token, status, lease_until,
	    accounting_day, reserved_execution_millis, started_at
	) VALUES (?, ?, ?, ?, ?, 'RUNNING', ?, CURRENT_DATE, ?, ?)`,
		candidateTenantID, jobInternalID, newAttemptNo, workerID, leaseToken, leaseUntil, reservedExecutionMillis, leaseNow); err != nil {
		return WorkerJobClaim{}, false, repositoryUnavailable("persist worker attempt", err)
	}
	if err := advanceTenantFairness(ctx, tx, candidateTenantID, leaseNow); err != nil {
		return WorkerJobClaim{}, false, err
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

func advanceTenantFairness(ctx context.Context, tx *sql.Tx, tenantID uint64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "UPDATE t_external_tenant SET last_claimed_at = ? WHERE id = ?", now, tenantID); err != nil {
		return repositoryUnavailable("advance fair tenant cursor", err)
	}
	return nil
}

func (repository *MySQLJobRepository) failImpossibleDailyExecution(
	ctx context.Context,
	tx *sql.Tx,
	tenantID, jobID uint64,
	status JobStatus,
	attemptNo uint32,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
	UPDATE t_external_job
	SET status = 'FAILED', failure_code = 'DAILY_EXECUTION_LIMIT_TOO_LOW', completed_at = ?,
	    worker_id = NULL, lease_token = NULL, lease_until = NULL
	WHERE tenant_id = ? AND id = ? AND status = ? AND attempt_no = ?`, now, tenantID, jobID, status, attemptNo)
	if err != nil {
		return repositoryUnavailable("fail impossible daily execution job", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrJobNotClaimable
	}
	if err := advanceTenantFairness(ctx, tx, tenantID, now); err != nil {
		return err
	}
	job, err := getExternalJobByInternalID(ctx, tx, jobID)
	if err != nil {
		return repositoryUnavailable("read impossible daily execution job", err)
	}
	if _, err := repository.insertTerminalWebhookEvent(ctx, tx, now, job); err != nil {
		return err
	}
	return nil
}

func expireAttempt(ctx context.Context, tx *sql.Tx, tenantID, jobID uint64, attemptNo uint32, now time.Time) error {
	if _, err := releaseAttemptReservation(ctx, tx, tenantID, jobID, attemptNo, nil); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE t_external_job_attempt
	SET status = 'EXPIRED', lease_token = NULL, finished_at = ?, failure_code = 'LEASE_EXPIRED',
	    reserved_execution_millis = 0
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
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin heartbeat", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return err
	}
	leaseUntil := now.Add(leaseDuration)
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

func (repository *MySQLJobRepository) ClaimCancelled(ctx context.Context, claim WorkerJobClaim) (bool, error) {
	if repository == nil || !validWorkerClaim(claim) {
		return false, ErrStaleJobClaim
	}
	var cancelled bool
	err := repository.database.QueryRowContext(ctx, `
SELECT cancel_requested_at IS NOT NULL
FROM t_external_job
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > CURRENT_TIMESTAMP(3)`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken).Scan(&cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrStaleJobClaim
	}
	if err != nil {
		return false, repositoryUnavailable("read active claim control", err)
	}
	return cancelled, nil
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
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return repositoryUnavailable("begin completion", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return err
	}
	cancelled, _, err := lockActiveClaimAndPolicy(ctx, tx, claim)
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
	consumedMillis := int64(0)
	if cancelled {
		result.Cases = nil
	}
	consumedMillis, err = settleAttemptReservation(ctx, tx, claim, result.Cases)
	if err != nil {
		return err
	}
	if err := finishAttempt(ctx, tx, claim, attemptStatus, "", consumedMillis, now); err != nil {
		return err
	}
	job, err := getExternalJobByInternalID(ctx, tx, claim.Job.InternalID)
	if err != nil {
		return repositoryUnavailable("read completed job", err)
	}
	if _, err := repository.insertTerminalWebhookEvent(ctx, tx, now, job); err != nil {
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
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", repositoryUnavailable("begin infrastructure failure", err)
	}
	defer tx.Rollback()
	now, err := mysqlCurrentTime(ctx, tx)
	if err != nil {
		return "", err
	}
	cancelled, encodedPolicy, err := lockActiveClaimAndPolicy(ctx, tx, claim)
	if err != nil {
		return "", err
	}
	policy, err := decodeTenantPolicy(encodedPolicy)
	if err != nil {
		return "", ErrExternalJobUnavailable
	}
	disposition := FailureTerminal
	jobStatus := JobStatusFailed
	failureCode := any(failure.Code)
	attemptFailureCode := failure.Code
	completedAt := any(now)
	nextAttemptAt := any(now)
	if cancelled {
		disposition = FailureCancelled
		jobStatus = JobStatusCancelled
		failureCode = nil
		attemptFailureCode = ""
	} else if int(claim.AttemptNo) < policy.MaxInfrastructureTries {
		disposition = FailureRequeued
		jobStatus = JobStatusQueued
		failureCode = nil
		completedAt = nil
		nextAttemptAt = now.Add(failure.RetryDelay)
	}
	jobResult, err := tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = ?, next_attempt_at = ?, failure_code = ?, completed_at = ?,
    worker_id = NULL, lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ? AND lease_token = ?`,
		jobStatus, nextAttemptAt, failureCode, completedAt,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return "", repositoryUnavailable("persist infrastructure failure", err)
	}
	if affected, err := jobResult.RowsAffected(); err != nil || affected != 1 {
		return "", ErrStaleJobClaim
	}
	if _, err := settleAttemptReservation(ctx, tx, claim, nil); err != nil {
		return "", err
	}
	attemptStatus := "FAILED"
	if cancelled {
		attemptStatus = "CANCELLED"
	}
	if err := finishAttempt(ctx, tx, claim, attemptStatus, attemptFailureCode, 0, now); err != nil {
		return "", err
	}
	if disposition != FailureRequeued {
		job, err := getExternalJobByInternalID(ctx, tx, claim.Job.InternalID)
		if err != nil {
			return "", repositoryUnavailable("read terminal failed job", err)
		}
		if _, err := repository.insertTerminalWebhookEvent(ctx, tx, now, job); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", repositoryUnavailable("commit infrastructure failure", err)
	}
	return disposition, nil
}

func lockActiveClaim(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim) (bool, error) {
	var cancelRequested sql.NullTime
	err := tx.QueryRowContext(ctx, `
SELECT cancel_requested_at FROM t_external_job
WHERE id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > CURRENT_TIMESTAMP(3) FOR UPDATE`,
		claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken).Scan(&cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrStaleJobClaim
	}
	if err != nil {
		return false, repositoryUnavailable("lock active claim", err)
	}
	return cancelRequested.Valid, nil
}

func lockActiveClaimAndPolicy(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim) (bool, []byte, error) {
	var tenantID uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT tenant_id FROM t_external_job WHERE id = ?",
		claim.Job.InternalID).Scan(&tenantID); errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrStaleJobClaim
	} else if err != nil {
		return false, nil, repositoryUnavailable("read active claim tenant", err)
	}
	var encodedPolicy []byte
	if err := tx.QueryRowContext(ctx,
		"SELECT policy_json FROM t_external_tenant WHERE id = ? FOR UPDATE",
		tenantID).Scan(&encodedPolicy); err != nil {
		return false, nil, repositoryUnavailable("lock active claim tenant", err)
	}
	var cancelRequested sql.NullTime
	err := tx.QueryRowContext(ctx, `
SELECT cancel_requested_at
FROM t_external_job
WHERE id = ? AND tenant_id = ? AND status = 'RUNNING' AND attempt_no = ? AND worker_id = ?
  AND lease_token = ? AND lease_until > CURRENT_TIMESTAMP(3) FOR UPDATE`,
		claim.Job.InternalID, tenantID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken).
		Scan(&cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrStaleJobClaim
	}
	if err != nil {
		return false, nil, repositoryUnavailable("lock active claim policy", err)
	}
	return cancelRequested.Valid, encodedPolicy, nil
}

func finishAttempt(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim, status, failureCode string, consumedMillis int64, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
	UPDATE t_external_job_attempt
	SET status = ?, lease_token = NULL, finished_at = ?, failure_code = NULLIF(?, ''),
	    reserved_execution_millis = 0, consumed_execution_millis = ?
	WHERE job_id = ? AND attempt_no = ? AND worker_id = ? AND lease_token = ? AND status = 'RUNNING'`,
		status, now, failureCode, consumedMillis, claim.Job.InternalID, claim.AttemptNo, claim.WorkerID, claim.LeaseToken)
	if err != nil {
		return repositoryUnavailable("finish worker attempt", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStaleJobClaim
	}
	return nil
}

func reserveDailyExecution(ctx context.Context, tx *sql.Tx, tenantID uint64, dailyLimit, reserveMillis int64) (dailyReservationDecision, error) {
	if dailyLimit <= 0 || reserveMillis <= 0 {
		return 0, ErrExternalJobUnavailable
	}
	if reserveMillis > dailyLimit {
		return dailyReservationImpossible, nil
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO t_external_execution_daily(tenant_id, accounting_day)
	VALUES (?, CURRENT_DATE)
	ON DUPLICATE KEY UPDATE tenant_id = VALUES(tenant_id)`, tenantID); err != nil {
		return 0, repositoryUnavailable("ensure daily execution ledger", err)
	}
	var reserved, consumed int64
	if err := tx.QueryRowContext(ctx, `
	SELECT reserved_millis, consumed_millis FROM t_external_execution_daily
	WHERE tenant_id = ? AND accounting_day = CURRENT_DATE FOR UPDATE`, tenantID).Scan(&reserved, &consumed); err != nil {
		return 0, repositoryUnavailable("lock daily execution ledger", err)
	}
	if consumed > dailyLimit-reserveMillis || reserved > dailyLimit-reserveMillis-consumed {
		return dailyReservationDeferred, nil
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE t_external_execution_daily SET reserved_millis = reserved_millis + ?
	WHERE tenant_id = ? AND accounting_day = CURRENT_DATE`, reserveMillis, tenantID)
	if err != nil {
		return 0, repositoryUnavailable("reserve daily execution", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return 0, repositoryUnavailable("reserve daily execution", ErrExternalJobUnavailable)
	}
	return dailyReservationAllowed, nil
}

func releaseAttemptReservation(ctx context.Context, tx *sql.Tx, tenantID, jobID uint64, attemptNo uint32, cases []DurableCaseResult) (int64, error) {
	var accountingDay sql.NullTime
	var reserved int64
	if err := tx.QueryRowContext(ctx, `
	SELECT accounting_day, reserved_execution_millis FROM t_external_job_attempt
	WHERE tenant_id = ? AND job_id = ? AND attempt_no = ? AND status = 'RUNNING' FOR UPDATE`, tenantID, jobID, attemptNo).
		Scan(&accountingDay, &reserved); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrStaleJobClaim
		}
		return 0, repositoryUnavailable("lock attempt execution reservation", err)
	}
	if !accountingDay.Valid || reserved <= 0 {
		return 0, ErrExternalJobUnavailable
	}
	consumedMillis := cappedCaseExecutionMillis(cases, reserved)
	result, err := tx.ExecContext(ctx, `
	UPDATE t_external_execution_daily
	SET reserved_millis = reserved_millis - ?, consumed_millis = consumed_millis + ?
	WHERE tenant_id = ? AND accounting_day = ? AND reserved_millis >= ?`, reserved, consumedMillis, tenantID, accountingDay.Time, reserved)
	if err != nil {
		return 0, repositoryUnavailable("settle daily execution ledger", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return 0, repositoryUnavailable("settle daily execution ledger", ErrExternalJobUnavailable)
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE t_external_job
	SET next_attempt_at = CURRENT_TIMESTAMP(3)
	WHERE tenant_id = ? AND status = 'QUEUED'
	  AND next_attempt_at = TIMESTAMP(DATE_ADD(?, INTERVAL 1 DAY))`, tenantID, accountingDay.Time); err != nil {
		return 0, repositoryUnavailable("wake daily quota jobs after settlement", err)
	}
	return consumedMillis, nil
}

func settleAttemptReservation(ctx context.Context, tx *sql.Tx, claim WorkerJobClaim, cases []DurableCaseResult) (int64, error) {
	return releaseAttemptReservation(ctx, tx, claim.Job.TenantInternalID, claim.Job.InternalID, claim.AttemptNo, cases)
}

func cappedCaseExecutionMillis(cases []DurableCaseResult, reservation int64) int64 {
	if reservation <= 0 {
		return 0
	}
	var total int64
	for _, item := range cases {
		if item.TimeMillis >= reservation-total {
			return reservation
		}
		total += item.TimeMillis
	}
	return total
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

func mysqlCurrentTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(3)").Scan(&now); err != nil {
		return time.Time{}, repositoryUnavailable("read database lease clock", err)
	}
	return now.UTC(), nil
}

func waitForClaimRetry(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
