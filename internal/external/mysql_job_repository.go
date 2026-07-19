package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const submitJobIdempotencyScope = "judge-job-submit"

type MySQLJobRepositoryConfig struct {
	Database          *sql.DB
	Random            io.Reader
	Now               func() time.Time
	IdempotencyPepper []byte
	CursorKey         []byte
	SourceCipher      *SourceCipher
	SourceObjects     SourceObjectStore
	IdempotencyTTL    time.Duration
}

type MySQLJobRepository struct {
	database          *sql.DB
	random            io.Reader
	now               func() time.Time
	idempotencyPepper []byte
	cursor            *JobCursorCodec
	sourceCipher      *SourceCipher
	sourceObjects     SourceObjectStore
	idempotencyTTL    time.Duration
}

func NewMySQLJobRepository(config MySQLJobRepositoryConfig) (*MySQLJobRepository, error) {
	if config.Database == nil || config.Random == nil || config.Now == nil || config.SourceCipher == nil || config.SourceObjects == nil {
		return nil, fmt.Errorf("database, random source, clock, source cipher, and source object store are required")
	}
	if len(config.IdempotencyPepper) < sha256.Size || config.IdempotencyTTL <= 0 || config.IdempotencyTTL > 7*24*time.Hour {
		return nil, fmt.Errorf("idempotency pepper and a retention window up to seven days are required")
	}
	cursor, err := NewJobCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	return &MySQLJobRepository{
		database: config.Database, random: config.Random, now: config.Now,
		idempotencyPepper: append([]byte(nil), config.IdempotencyPepper...), cursor: cursor,
		sourceCipher: config.SourceCipher, sourceObjects: config.SourceObjects,
		idempotencyTTL: config.IdempotencyTTL,
	}, nil
}

func (repository *MySQLJobRepository) Submit(
	ctx context.Context,
	tenantExternalID string,
	idempotencyKey string,
	request JudgeJobRequest,
) (SubmitJobResult, error) {
	if repository == nil || !externalIDPattern.MatchString(tenantExternalID) {
		return SubmitJobResult{}, ErrExternalJobInvalid
	}
	keyDigest, err := DigestIdempotencyKey(idempotencyKey, repository.idempotencyPepper)
	if err != nil {
		return SubmitJobResult{}, fmt.Errorf("%w: invalid idempotency key", ErrExternalJobInvalid)
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SubmitJobResult{}, repositoryUnavailable("begin submit transaction", err)
	}
	defer tx.Rollback()

	tenantInternalID, policy, err := lockTenantPolicy(ctx, tx, tenantExternalID)
	if err != nil {
		return SubmitJobResult{}, err
	}
	// Canonical identity must remain stable when an operator tightens policy.
	// Current admission limits are applied only after an idempotent replay has
	// had a chance to return its already-accepted resource.
	requestHash, err := CanonicalJobRequestHash(request, int64(len(request.SourceCode)))
	if err != nil {
		return SubmitJobResult{}, fmt.Errorf("%w: %v", ErrExternalJobInvalid, err)
	}
	now := repository.now().UTC()
	if _, err := tx.ExecContext(ctx, `
DELETE FROM t_external_idempotency
WHERE tenant_id = ? AND operation_scope = ? AND key_digest = ? AND expires_at <= ?`,
		tenantInternalID, submitJobIdempotencyScope, keyDigest, now); err != nil {
		return SubmitJobResult{}, repositoryUnavailable("expire idempotency record", err)
	}
	var storedHash []byte
	var existingJobID string
	err = tx.QueryRowContext(ctx, `
SELECT request_hash, resource_external_id
FROM t_external_idempotency
WHERE tenant_id = ? AND operation_scope = ? AND key_digest = ?`,
		tenantInternalID, submitJobIdempotencyScope, keyDigest).Scan(&storedHash, &existingJobID)
	if err == nil {
		if !bytes.Equal(storedHash, requestHash) {
			return SubmitJobResult{}, ErrExternalJobConflict
		}
		job, err := getExternalJob(ctx, tx, tenantExternalID, existingJobID, false)
		if err != nil {
			return SubmitJobResult{}, repositoryUnavailable("read replayed job", err)
		}
		if err := tx.Commit(); err != nil {
			return SubmitJobResult{}, repositoryUnavailable("commit idempotent replay", err)
		}
		return SubmitJobResult{Job: job, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SubmitJobResult{}, repositoryUnavailable("read idempotency record", err)
	}
	if int64(len(request.SourceCode)) > policy.MaxSourceBytes {
		return SubmitJobResult{}, fmt.Errorf("%w: source code exceeds current tenant policy", ErrExternalJobInvalid)
	}

	var queuedJobs int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM t_external_job WHERE tenant_id = ? AND status = 'QUEUED'",
		tenantInternalID).Scan(&queuedJobs); err != nil {
		return SubmitJobResult{}, repositoryUnavailable("establish queued quota", err)
	}
	if queuedJobs >= policy.MaxQueuedJobs {
		return SubmitJobResult{}, ErrQueuedQuotaExceeded
	}

	var bundleInternalID uint64
	if err := tx.QueryRowContext(ctx, `
SELECT id FROM t_external_bundle
WHERE tenant_id = ? AND external_id = ? AND ready_at IS NOT NULL
  AND delete_marked_at IS NULL AND deleted_at IS NULL`,
		tenantInternalID, request.BundleID).Scan(&bundleInternalID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubmitJobResult{}, fmt.Errorf("%w: bundle is unavailable", ErrExternalJobInvalid)
		}
		return SubmitJobResult{}, repositoryUnavailable("read tenant bundle", err)
	}
	var callbackInternalID sql.NullInt64
	if request.CallbackID != "" {
		var callbackID int64
		if err := tx.QueryRowContext(ctx, `
SELECT id FROM t_external_callback
WHERE tenant_id = ? AND external_id = ? AND disabled_at IS NULL`,
			tenantInternalID, request.CallbackID).Scan(&callbackID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return SubmitJobResult{}, fmt.Errorf("%w: callback is unavailable", ErrExternalJobInvalid)
			}
			return SubmitJobResult{}, repositoryUnavailable("read tenant callback", err)
		}
		callbackInternalID = sql.NullInt64{Int64: callbackID, Valid: true}
	}

	sourceExternalID, err := generateExternalID(repository.random)
	if err != nil {
		return SubmitJobResult{}, repositoryUnavailable("generate source object ID", err)
	}
	jobExternalID, err := generateExternalID(repository.random)
	if err != nil {
		return SubmitJobResult{}, repositoryUnavailable("generate job ID", err)
	}
	sourceObjectKey, err := SourceObjectKey(tenantExternalID, sourceExternalID)
	if err != nil {
		return SubmitJobResult{}, repositoryUnavailable("derive source object key", err)
	}
	encrypted, err := repository.sourceCipher.Encrypt(tenantExternalID, sourceExternalID, request.SourceCode)
	if err != nil {
		return SubmitJobResult{}, repositoryUnavailable("encrypt source", err)
	}
	if _, err := repository.database.ExecContext(ctx,
		"INSERT INTO t_external_source_reservation(object_key) VALUES (?)", sourceObjectKey); err != nil {
		return SubmitJobResult{}, repositoryUnavailable("reserve encrypted source object", err)
	}
	reservationActive := true
	releaseReservation := func() error {
		if !reservationActive {
			return nil
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := repository.database.ExecContext(cleanupContext,
			"DELETE FROM t_external_source_reservation WHERE object_key = ?", sourceObjectKey); err != nil {
			return repositoryUnavailable("release encrypted source reservation", err)
		}
		reservationActive = false
		return nil
	}
	if err := repository.sourceObjects.Create(ctx, sourceObjectKey, encrypted.Ciphertext); err != nil {
		// Object-store SDK errors commonly embed bucket/key/request details.
		// Preserve the availability class without making those details loggable.
		// The durable reservation remains because an object-store timeout can be
		// outcome-ambiguous; the sweeper will check DB ownership before deletion.
		return SubmitJobResult{}, fmt.Errorf("%w: persist encrypted source", ErrExternalJobUnavailable)
	}
	objectPublished := true
	cleanupObject := func(cause error) error {
		if !objectPublished {
			return cause
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanupErr := repository.sourceObjects.Delete(cleanupContext, sourceObjectKey); cleanupErr != nil {
			return fmt.Errorf("%w: source compensation failed", ErrExternalJobUnavailable)
		}
		objectPublished = false
		if cleanupErr := releaseReservation(); cleanupErr != nil {
			return cleanupErr
		}
		return cause
	}
	sourceResult, err := tx.ExecContext(ctx, `
INSERT INTO t_external_source_object(
    external_id, tenant_id, object_key, source_sha256, source_size_bytes,
    encryption_key_version, encryption_nonce
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sourceExternalID, tenantInternalID, sourceObjectKey, encrypted.SHA256, encrypted.SizeBytes,
		encrypted.KeyVersion, encrypted.Nonce)
	if err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("persist source metadata", err))
	}
	sourceInternalID, err := sourceResult.LastInsertId()
	if err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("read source metadata ID", err))
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO t_external_job(
    external_id, tenant_id, bundle_id, source_object_id, callback_id, status,
    language_id, stop_on_failure, client_reference, request_hash, next_attempt_at, created_at
) VALUES (?, ?, ?, ?, ?, 'QUEUED', ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		jobExternalID, tenantInternalID, bundleInternalID, sourceInternalID, callbackInternalID,
		request.Language, request.StopOnFailure, request.ClientReference, requestHash, now, now); err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("persist queued job", err))
	}
	responseJSON, err := json.Marshal(struct {
		JobID     string    `json:"jobId"`
		Status    JobStatus `json:"status"`
		CreatedAt time.Time `json:"createdAt"`
	}{JobID: jobExternalID, Status: JobStatusQueued, CreatedAt: now})
	if err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("encode idempotency response", err))
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO t_external_idempotency(
    tenant_id, operation_scope, key_digest, request_hash, resource_type,
    resource_external_id, response_status, response_json, expires_at
) VALUES (?, ?, ?, ?, 'judge-job', ?, 202, ?, ?)`,
		tenantInternalID, submitJobIdempotencyScope, keyDigest, requestHash,
		jobExternalID, responseJSON, now.Add(repository.idempotencyTTL)); err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("persist idempotency record", err))
	}
	job, err := getExternalJob(ctx, tx, tenantExternalID, jobExternalID, false)
	if err != nil {
		return SubmitJobResult{}, cleanupObject(repositoryUnavailable("read submitted job", err))
	}
	if err := tx.Commit(); err != nil {
		// Commit errors can be outcome-ambiguous (the server may have committed
		// before the connection failed). Deleting here could break a committed
		// job. Keep this private, content-opaque object for the retention sweeper;
		// an idempotent retry determines whether the transaction committed.
		return SubmitJobResult{}, repositoryUnavailable("commit submitted job with unknown outcome", err)
	}
	objectPublished = false
	// A stale reservation is safe: the sweeper observes the authoritative
	// source metadata and removes only the reservation, never the live object.
	_ = releaseReservation()
	return SubmitJobResult{Job: job}, nil
}

func lockTenantPolicy(ctx context.Context, tx *sql.Tx, tenantExternalID string) (uint64, TenantPolicy, error) {
	var tenantInternalID uint64
	var status string
	var encodedPolicy []byte
	if err := tx.QueryRowContext(ctx, `
SELECT id, status, policy_json FROM t_external_tenant
WHERE external_id = ? FOR UPDATE`, tenantExternalID).Scan(&tenantInternalID, &status, &encodedPolicy); err != nil {
		return 0, TenantPolicy{}, repositoryUnavailable("lock tenant policy", err)
	}
	if status != "ACTIVE" {
		return 0, TenantPolicy{}, ErrExternalJobUnavailable
	}
	var policy TenantPolicy
	decoder := json.NewDecoder(bytes.NewReader(encodedPolicy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil || policy.validate() != nil {
		return 0, TenantPolicy{}, ErrExternalJobUnavailable
	}
	return tenantInternalID, policy, nil
}

func (repository *MySQLJobRepository) Get(ctx context.Context, tenantExternalID, jobExternalID string) (ExternalJobRecord, error) {
	if repository == nil || !externalIDPattern.MatchString(tenantExternalID) || !externalIDPattern.MatchString(jobExternalID) {
		return ExternalJobRecord{}, ErrExternalJobNotFound
	}
	job, err := getExternalJob(ctx, repository.database, tenantExternalID, jobExternalID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalJobRecord{}, ErrExternalJobNotFound
	}
	if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("read job", err)
	}
	return job, nil
}

func (repository *MySQLJobRepository) List(ctx context.Context, tenantExternalID string, options JobListOptions) (JobListResult, error) {
	if repository == nil || !externalIDPattern.MatchString(tenantExternalID) || options.Limit < 1 || options.Limit > 100 || !validJobStatusFilter(options.Status) {
		return JobListResult{}, ErrExternalJobInvalid
	}
	arguments := []any{tenantExternalID}
	conditions := []string{"tenant.external_id = ?"}
	if options.Status != "" {
		conditions = append(conditions, "job.status = ?")
		arguments = append(arguments, options.Status)
	}
	if options.Cursor != "" {
		cursor, err := repository.cursor.Decode(options.Cursor, tenantExternalID, options.Status)
		if err != nil {
			return JobListResult{}, ErrInvalidJobCursor
		}
		conditions = append(conditions, "(job.created_at < ? OR (job.created_at = ? AND job.id < ?))")
		arguments = append(arguments, cursor.CreatedAt, cursor.CreatedAt, cursor.InternalID)
	}
	arguments = append(arguments, options.Limit+1)
	query := externalJobSelect + " WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY job.created_at DESC, job.id DESC LIMIT ?"
	rows, err := repository.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return JobListResult{}, repositoryUnavailable("list jobs", err)
	}
	defer rows.Close()
	jobs := make([]ExternalJobRecord, 0, options.Limit+1)
	for rows.Next() {
		job, err := scanExternalJob(rows)
		if err != nil {
			return JobListResult{}, repositoryUnavailable("scan listed job", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return JobListResult{}, repositoryUnavailable("iterate listed jobs", err)
	}
	result := JobListResult{Jobs: jobs}
	if len(jobs) > options.Limit {
		last := jobs[options.Limit-1]
		result.Jobs = jobs[:options.Limit]
		result.NextCursor, err = repository.cursor.Encode(JobCursor{
			TenantID: tenantExternalID, Status: options.Status,
			CreatedAt: last.CreatedAt, InternalID: last.InternalID,
		})
		if err != nil {
			return JobListResult{}, repositoryUnavailable("encode next cursor", err)
		}
	}
	return result, nil
}

func (repository *MySQLJobRepository) Cancel(ctx context.Context, tenantExternalID, jobExternalID string) (ExternalJobRecord, error) {
	if repository == nil || !externalIDPattern.MatchString(tenantExternalID) || !externalIDPattern.MatchString(jobExternalID) {
		return ExternalJobRecord{}, ErrExternalJobNotFound
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("begin cancellation", err)
	}
	defer tx.Rollback()
	// All mutating paths use tenant -> job lock order. This avoids a cancel
	// racing a worker claim into the inverse job -> tenant deadlock pattern.
	var tenantInternalID uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM t_external_tenant WHERE external_id = ? FOR UPDATE",
		tenantExternalID).Scan(&tenantInternalID); errors.Is(err, sql.ErrNoRows) {
		return ExternalJobRecord{}, ErrExternalJobNotFound
	} else if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("lock cancellation tenant", err)
	}
	job, err := getExternalJob(ctx, tx, tenantExternalID, jobExternalID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalJobRecord{}, ErrExternalJobNotFound
	}
	if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("lock job for cancellation", err)
	}
	now := repository.now().UTC()
	switch job.Status {
	case JobStatusQueued:
		_, err = tx.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'CANCELLED', cancel_requested_at = ?, completed_at = ?, worker_id = NULL,
    lease_token = NULL, lease_until = NULL
WHERE id = ? AND status = 'QUEUED'`, now, now, job.InternalID)
	case JobStatusRunning:
		if job.CancelRequested == nil {
			_, err = tx.ExecContext(ctx, `UPDATE t_external_job SET cancel_requested_at = ? WHERE id = ? AND status = 'RUNNING'`, now, job.InternalID)
		}
	case JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
	default:
		return ExternalJobRecord{}, ErrExternalJobUnavailable
	}
	if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("persist cancellation", err)
	}
	job, err = getExternalJob(ctx, tx, tenantExternalID, jobExternalID, false)
	if err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("read cancelled job", err)
	}
	if err := tx.Commit(); err != nil {
		return ExternalJobRecord{}, repositoryUnavailable("commit cancellation", err)
	}
	return job, nil
}

type rowScannerSQL interface{ Scan(...any) error }

type queryerSQL interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const externalJobSelect = `
SELECT job.id, job.external_id, tenant.external_id, bundle.external_id,
       source.external_id, source.object_key, source.source_sha256,
       source.source_size_bytes, source.encryption_key_version, source.encryption_nonce,
       callback.external_id, job.status, job.language_id, job.stop_on_failure,
       job.client_reference, job.attempt_no, job.worker_id, job.lease_until,
       job.cancel_requested_at, job.result_json, job.failure_code,
       job.created_at, job.started_at, job.completed_at
FROM t_external_job AS job
JOIN t_external_tenant AS tenant ON tenant.id = job.tenant_id
JOIN t_external_bundle AS bundle ON bundle.id = job.bundle_id AND bundle.tenant_id = job.tenant_id
JOIN t_external_source_object AS source ON source.id = job.source_object_id AND source.tenant_id = job.tenant_id
LEFT JOIN t_external_callback AS callback ON callback.id = job.callback_id AND callback.tenant_id = job.tenant_id`

func getExternalJob(ctx context.Context, queryer queryerSQL, tenantExternalID, jobExternalID string, forUpdate bool) (ExternalJobRecord, error) {
	query := externalJobSelect + " WHERE tenant.external_id = ? AND job.external_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanExternalJob(queryer.QueryRowContext(ctx, query, tenantExternalID, jobExternalID))
}

func scanExternalJob(scanner rowScannerSQL) (ExternalJobRecord, error) {
	var job ExternalJobRecord
	var callbackID, clientReference, workerID, failureCode sql.NullString
	var leaseUntil, cancelRequested, startedAt, completedAt sql.NullTime
	var resultJSON []byte
	var keyVersion uint64
	if err := scanner.Scan(
		&job.InternalID, &job.ExternalID, &job.TenantExternalID, &job.BundleExternalID,
		&job.Source.ExternalID, &job.Source.ObjectKey, &job.Source.SHA256,
		&job.Source.SizeBytes, &keyVersion, &job.Source.Nonce,
		&callbackID, &job.Status, &job.Language, &job.StopOnFailure,
		&clientReference, &job.AttemptNo, &workerID, &leaseUntil,
		&cancelRequested, &resultJSON, &failureCode,
		&job.CreatedAt, &startedAt, &completedAt,
	); err != nil {
		return ExternalJobRecord{}, err
	}
	if keyVersion == 0 || keyVersion > 65535 {
		return ExternalJobRecord{}, fmt.Errorf("invalid source key version")
	}
	job.Source.KeyVersion = uint16(keyVersion)
	job.CallbackID = callbackID.String
	job.ClientReference = clientReference.String
	job.WorkerID = workerID.String
	job.FailureCode = failureCode.String
	job.LeaseUntil = nullableTimePointer(leaseUntil)
	job.CancelRequested = nullableTimePointer(cancelRequested)
	job.StartedAt = nullableTimePointer(startedAt)
	job.CompletedAt = nullableTimePointer(completedAt)
	if len(resultJSON) > 0 {
		var result DurableJobResult
		decoder := json.NewDecoder(bytes.NewReader(resultJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil || validateDurableJobResult(result) != nil {
			return ExternalJobRecord{}, fmt.Errorf("invalid persisted job result")
		}
		job.Result = &result
	}
	return job, nil
}

func nullableTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	copied := value.Time.UTC()
	return &copied
}

func validJobStatusFilter(status JobStatus) bool {
	switch status {
	case "", JobStatusQueued, JobStatusRunning, JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
		return true
	default:
		return false
	}
}

func repositoryUnavailable(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrExternalJobUnavailable, operation, err)
}
