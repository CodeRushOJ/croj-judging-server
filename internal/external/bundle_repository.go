package external

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

const bundleUploadOperationScope = "bundle-upload"

var bundlePublicationFailureCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

type SQLBundleRepository struct{ database *sql.DB }

func NewSQLBundleRepository(database *sql.DB) (*SQLBundleRepository, error) {
	if database == nil {
		return nil, fmt.Errorf("bundle database is required")
	}
	return &SQLBundleRepository{database: database}, nil
}

func (repository *SQLBundleRepository) FindBundleUpload(ctx context.Context, tenantID string, keyDigest [sha256.Size]byte) (BundleUploadLookup, error) {
	if repository == nil || repository.database == nil || !externalIDPattern.MatchString(tenantID) {
		return BundleUploadLookup{}, fmt.Errorf("bundle repository is not configured")
	}
	var requestHash []byte
	var responseJSON []byte
	var status string
	var stagingKey sql.NullString
	err := repository.database.QueryRowContext(ctx, `
SELECT idempotency.request_hash, idempotency.response_json, bundle.publication_status, bundle.staging_object_key
FROM t_external_idempotency AS idempotency
JOIN t_external_tenant AS tenant ON tenant.id = idempotency.tenant_id
JOIN t_external_bundle AS bundle
  ON bundle.tenant_id = idempotency.tenant_id
 AND bundle.external_id = idempotency.resource_external_id
WHERE tenant.external_id = ? AND tenant.status = 'ACTIVE'
  AND idempotency.operation_scope = ? AND idempotency.key_digest = ?
  AND idempotency.expires_at > UTC_TIMESTAMP(3)
  AND bundle.deleted_at IS NULL
LIMIT 1`, tenantID, bundleUploadOperationScope, keyDigest[:]).Scan(&requestHash, &responseJSON, &status, &stagingKey)
	if errors.Is(err, sql.ErrNoRows) {
		return BundleUploadLookup{}, nil
	}
	if err != nil {
		return BundleUploadLookup{}, fmt.Errorf("find bundle upload: %w", err)
	}
	if len(requestHash) != sha256.Size {
		return BundleUploadLookup{}, fmt.Errorf("stored bundle idempotency hash is invalid")
	}
	var metadata BundleMetadata
	if err := json.Unmarshal(responseJSON, &metadata); err != nil || !validBundleMetadata(metadata) {
		return BundleUploadLookup{}, fmt.Errorf("stored bundle idempotency response is invalid")
	}
	var digest [sha256.Size]byte
	copy(digest[:], requestHash)
	publicationStatus := BundlePublicationStatus(status)
	if !validBundlePublicationStatus(publicationStatus) {
		return BundleUploadLookup{}, fmt.Errorf("stored bundle publication status is invalid")
	}
	return BundleUploadLookup{Found: true, Status: publicationStatus, RequestHash: digest, StagingKey: stagingKey.String, Metadata: metadata}, nil
}

func (repository *SQLBundleRepository) CommitBundleUpload(ctx context.Context, input BundleCommitInput) (result BundleCommitResult, resultErr error) {
	if repository == nil || repository.database == nil || !validBundleCommitInput(input) {
		return BundleCommitResult{}, fmt.Errorf("bundle commit input is invalid")
	}
	transaction, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return BundleCommitResult{}, fmt.Errorf("begin bundle commit: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = transaction.Rollback()
		}
	}()

	var tenantInternalID uint64
	var encodedPolicy []byte
	if err := transaction.QueryRowContext(ctx, `
SELECT id, policy_json FROM t_external_tenant
WHERE external_id = ? AND status = 'ACTIVE'
FOR UPDATE`, input.TenantID).Scan(&tenantInternalID, &encodedPolicy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BundleCommitResult{}, ErrBundleNotFound
		}
		return BundleCommitResult{}, fmt.Errorf("lock bundle tenant: %w", err)
	}
	policy, err := decodeTenantPolicy(encodedPolicy)
	if err != nil {
		return BundleCommitResult{}, fmt.Errorf("bundle tenant policy is invalid")
	}
	if !bundleWithinTenantPolicy(input, policy) {
		return BundleCommitResult{}, fmt.Errorf("%w: execution limits exceed tenant maximum", ErrInvalidBundle)
	}
	if _, err := transaction.ExecContext(ctx, `
DELETE FROM t_external_idempotency
WHERE tenant_id = ? AND operation_scope = ? AND key_digest = ? AND expires_at <= UTC_TIMESTAMP(3)`,
		tenantInternalID, bundleUploadOperationScope, input.IdempotencyDigest[:]); err != nil {
		return BundleCommitResult{}, fmt.Errorf("remove expired bundle idempotency: %w", err)
	}

	var storedRequestHash []byte
	var storedResponse []byte
	var storedStatus string
	var storedStagingKey sql.NullString
	err = transaction.QueryRowContext(ctx, `
SELECT idempotency.request_hash, idempotency.response_json, bundle.publication_status, bundle.staging_object_key
FROM t_external_idempotency AS idempotency
JOIN t_external_bundle AS bundle
  ON bundle.tenant_id = idempotency.tenant_id
 AND bundle.external_id = idempotency.resource_external_id
WHERE idempotency.tenant_id = ? AND idempotency.operation_scope = ? AND idempotency.key_digest = ?
  AND bundle.deleted_at IS NULL
FOR UPDATE`, tenantInternalID, bundleUploadOperationScope, input.IdempotencyDigest[:]).Scan(&storedRequestHash, &storedResponse, &storedStatus, &storedStagingKey)
	if err == nil {
		if len(storedRequestHash) != sha256.Size {
			return BundleCommitResult{}, fmt.Errorf("stored bundle idempotency hash is invalid")
		}
		var storedDigest [sha256.Size]byte
		copy(storedDigest[:], storedRequestHash)
		if storedDigest != input.RequestHash {
			return BundleCommitResult{}, ErrIdempotencyConflict
		}
		var metadata BundleMetadata
		if err := json.Unmarshal(storedResponse, &metadata); err != nil || !validBundleMetadata(metadata) {
			return BundleCommitResult{}, fmt.Errorf("stored bundle idempotency response is invalid")
		}
		status := BundlePublicationStatus(storedStatus)
		if !validBundlePublicationStatus(status) {
			return BundleCommitResult{}, fmt.Errorf("stored bundle publication status is invalid")
		}
		stagingKey := storedStagingKey.String
		if bundlePublicationNeedsFreshStaging(status, stagingKey) {
			if err := reviveBundlePublication(ctx, transaction, tenantInternalID, metadata.BundleID, input); err != nil {
				return BundleCommitResult{}, err
			}
			status = BundlePublicationPending
			stagingKey = input.StagingObjectKey
		}
		if err := transaction.Commit(); err != nil {
			return BundleCommitResult{}, fmt.Errorf("commit bundle replay: %w", err)
		}
		return BundleCommitResult{Metadata: metadata, Replay: true, Status: status, StagingKey: stagingKey}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BundleCommitResult{}, fmt.Errorf("read bundle idempotency under lock: %w", err)
	}

	metadata, status, stagingKey, found, err := findBundleByDigest(ctx, transaction, tenantInternalID, input.RequestHash)
	if err != nil {
		return BundleCommitResult{}, err
	}
	if !found {
		metadata = input.Metadata
		if _, err := transaction.ExecContext(ctx, `
INSERT INTO t_external_bundle(
    external_id, tenant_id, sha256, object_key, staging_object_key, size_bytes,
    case_count, manifest_version, manifest_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, metadata.BundleID, tenantInternalID, input.RequestHash[:], input.ObjectKey, input.StagingObjectKey,
			metadata.SizeBytes, metadata.CaseCount, metadata.ManifestVersion, input.ManifestJSON, metadata.CreatedAt); err != nil {
			return BundleCommitResult{}, fmt.Errorf("insert immutable bundle metadata: %w", err)
		}
	} else {
		if bundlePublicationNeedsFreshStaging(status, stagingKey) {
			if err := reviveBundlePublication(ctx, transaction, tenantInternalID, metadata.BundleID, input); err != nil {
				return BundleCommitResult{}, err
			}
			status = BundlePublicationPending
			stagingKey = input.StagingObjectKey
		} else if _, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle SET delete_marked_at = NULL
WHERE tenant_id = ? AND external_id = ? AND deleted_at IS NULL`, tenantInternalID, metadata.BundleID); err != nil {
			return BundleCommitResult{}, fmt.Errorf("retain existing immutable bundle: %w", err)
		}
	}
	if !found {
		status = BundlePublicationPending
		stagingKey = input.StagingObjectKey
	}
	responseJSON, err := json.Marshal(metadata)
	if err != nil {
		return BundleCommitResult{}, fmt.Errorf("encode immutable bundle response: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
INSERT INTO t_external_idempotency(
    tenant_id, operation_scope, key_digest, request_hash, resource_type,
    resource_external_id, response_status, response_json, expires_at
) VALUES (?, ?, ?, ?, 'bundle', ?, ?, ?, ?)`, tenantInternalID, bundleUploadOperationScope,
		input.IdempotencyDigest[:], input.RequestHash[:], metadata.BundleID, statusForBundleCommit(found), responseJSON, input.IdempotencyExpiresAt); err != nil {
		return BundleCommitResult{}, fmt.Errorf("insert bundle idempotency: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return BundleCommitResult{}, fmt.Errorf("commit immutable bundle: %w", err)
	}
	return BundleCommitResult{Metadata: metadata, Replay: found, Status: status, StagingKey: stagingKey}, nil
}

func reviveBundlePublication(ctx context.Context, transaction *sql.Tx, tenantInternalID uint64, bundleID string, input BundleCommitInput) error {
	result, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle
SET staging_object_key = ?, publication_status = 'PENDING', ready_at = NULL,
    publish_lease_token = NULL, publish_lease_until = NULL, publish_attempt_count = 0,
    publish_next_attempt_at = ?, publish_last_error_code = NULL, publish_abandoned_at = NULL,
    delete_marked_at = NULL
WHERE tenant_id = ? AND external_id = ? AND sha256 = ? AND deleted_at IS NULL
  AND (publication_status = 'ABANDONED' OR staging_object_key IS NULL)`,
		input.StagingObjectKey, input.Metadata.CreatedAt, tenantInternalID, bundleID, input.RequestHash[:])
	if err != nil {
		return fmt.Errorf("revive immutable bundle publication: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf("revive immutable bundle publication: state changed concurrently")
	}
	return nil
}

func (repository *SQLBundleRepository) ClaimBundlePublication(ctx context.Context, tenantID, bundleID, leaseToken string, now, leaseUntil time.Time) (BundlePublicationClaim, bool, error) {
	if repository == nil || repository.database == nil || !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(bundleID) || !externalIDPattern.MatchString(leaseToken) || !leaseUntil.After(now) {
		return BundlePublicationClaim{}, false, fmt.Errorf("bundle publication claim is invalid")
	}
	transaction, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("begin bundle publication claim: %w", err)
	}
	defer transaction.Rollback()
	var tenantInternalID uint64
	if err := transaction.QueryRowContext(ctx, `SELECT id FROM t_external_tenant WHERE external_id = ? AND status = 'ACTIVE' FOR UPDATE`, tenantID).Scan(&tenantInternalID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BundlePublicationClaim{}, false, ErrBundleNotFound
		}
		return BundlePublicationClaim{}, false, fmt.Errorf("find bundle publication tenant: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle
SET publication_status = 'PUBLISHING', publish_lease_token = ?, publish_lease_until = ?,
    publish_attempt_count = publish_attempt_count + 1, publish_last_error_code = NULL
WHERE tenant_id = ? AND external_id = ? AND staging_object_key IS NOT NULL
  AND deleted_at IS NULL AND publish_abandoned_at IS NULL
  AND ((publication_status = 'PENDING' AND publish_next_attempt_at <= ?)
    OR (publication_status = 'PUBLISHING' AND publish_lease_until <= ?))`,
		leaseToken, leaseUntil, tenantInternalID, bundleID, now, now)
	if err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("claim bundle publication: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("read bundle publication claim result: %w", err)
	}
	if affected != 1 {
		_ = transaction.Rollback()
		return BundlePublicationClaim{}, false, nil
	}
	claim, err := scanBundlePublicationClaim(transaction.QueryRowContext(ctx, `
SELECT tenant.external_id, bundle.external_id, bundle.object_key, bundle.staging_object_key,
       bundle.sha256, bundle.size_bytes, bundle.publish_lease_token, bundle.publish_attempt_count
FROM t_external_bundle AS bundle
JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id
WHERE bundle.tenant_id = ? AND bundle.external_id = ? AND bundle.publish_lease_token = ?
LIMIT 1`, tenantInternalID, bundleID, leaseToken))
	if err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("load bundle publication claim: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("commit bundle publication claim: %w", err)
	}
	return claim, true, nil
}

func (repository *SQLBundleRepository) ClaimNextBundlePublication(ctx context.Context, leaseToken string, now, leaseUntil time.Time) (BundlePublicationClaim, bool, error) {
	if repository == nil || repository.database == nil || !externalIDPattern.MatchString(leaseToken) || !leaseUntil.After(now) {
		return BundlePublicationClaim{}, false, fmt.Errorf("next bundle publication claim is invalid")
	}
	var tenantID, bundleID string
	err := repository.database.QueryRowContext(ctx, `
SELECT tenant.external_id, bundle.external_id
FROM t_external_bundle AS bundle
JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id
WHERE tenant.status = 'ACTIVE' AND bundle.staging_object_key IS NOT NULL
  AND bundle.deleted_at IS NULL AND bundle.publish_abandoned_at IS NULL
  AND ((bundle.publication_status = 'PENDING' AND bundle.publish_next_attempt_at <= ?)
    OR (bundle.publication_status = 'PUBLISHING' AND bundle.publish_lease_until <= ?))
ORDER BY bundle.publish_next_attempt_at, bundle.id
LIMIT 1`, now, now).Scan(&tenantID, &bundleID)
	if errors.Is(err, sql.ErrNoRows) {
		return BundlePublicationClaim{}, false, nil
	}
	if err != nil {
		return BundlePublicationClaim{}, false, fmt.Errorf("select next bundle publication: %w", err)
	}
	return repository.ClaimBundlePublication(ctx, tenantID, bundleID, leaseToken, now, leaseUntil)
}

func (repository *SQLBundleRepository) CompleteBundlePublication(ctx context.Context, claim BundlePublicationClaim, readyAt time.Time) error {
	if !validBundlePublicationClaim(claim) || readyAt.IsZero() {
		return fmt.Errorf("bundle publication completion is invalid")
	}
	transaction, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin bundle publication completion: %w", err)
	}
	defer transaction.Rollback()
	var tenantInternalID uint64
	if err := transaction.QueryRowContext(ctx, `SELECT id FROM t_external_tenant WHERE external_id = ? AND status = 'ACTIVE' FOR UPDATE`, claim.TenantID).Scan(&tenantInternalID); err != nil {
		return fmt.Errorf("lock bundle publication tenant: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle
SET publication_status = 'READY', ready_at = ?, publish_lease_token = NULL,
    publish_lease_until = NULL, publish_last_error_code = NULL
WHERE tenant_id = ? AND external_id = ? AND sha256 = ? AND publication_status = 'PUBLISHING'
  AND publish_lease_token = ? AND deleted_at IS NULL`, readyAt, tenantInternalID, claim.BundleID, claim.RequestHash[:], claim.LeaseToken)
	if err != nil {
		return fmt.Errorf("complete bundle publication: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrBundlePublishing
	}
	return transaction.Commit()
}

func (repository *SQLBundleRepository) FailBundlePublication(ctx context.Context, claim BundlePublicationClaim, errorCode string, nextAttempt time.Time, maxAttempts int) (bool, error) {
	if !validBundlePublicationClaim(claim) || !validFailureCode(errorCode) || nextAttempt.IsZero() || maxAttempts <= 0 {
		return false, fmt.Errorf("bundle publication failure is invalid")
	}
	abandoned := claim.AttemptCount >= maxAttempts
	status := BundlePublicationPending
	var abandonedAt any
	if abandoned {
		status = BundlePublicationAbandoned
		abandonedAt = nextAttempt
	}
	transaction, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin bundle publication failure: %w", err)
	}
	defer transaction.Rollback()
	var tenantInternalID uint64
	if err := transaction.QueryRowContext(ctx, `SELECT id FROM t_external_tenant WHERE external_id = ? AND status = 'ACTIVE' FOR UPDATE`, claim.TenantID).Scan(&tenantInternalID); err != nil {
		return false, fmt.Errorf("lock failed bundle publication tenant: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle
SET publication_status = ?, publish_lease_token = NULL, publish_lease_until = NULL,
    publish_next_attempt_at = ?, publish_last_error_code = ?, publish_abandoned_at = ?
WHERE tenant_id = ? AND external_id = ? AND sha256 = ? AND publication_status = 'PUBLISHING'
  AND publish_lease_token = ? AND deleted_at IS NULL`, status, nextAttempt, errorCode, abandonedAt,
		tenantInternalID, claim.BundleID, claim.RequestHash[:], claim.LeaseToken)
	if err != nil {
		return false, fmt.Errorf("record bundle publication failure: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return false, ErrBundlePublishing
	}
	if err := transaction.Commit(); err != nil {
		return false, fmt.Errorf("commit bundle publication failure: %w", err)
	}
	return abandoned, nil
}

func (repository *SQLBundleRepository) SweepUnrecoverableBundlePublications(ctx context.Context, before time.Time, limit int) (int64, error) {
	if repository == nil || repository.database == nil || before.IsZero() || limit <= 0 || limit > 1000 {
		return 0, fmt.Errorf("bundle publication sweep is invalid")
	}
	query := fmt.Sprintf(`
UPDATE t_external_bundle
SET publication_status = 'ABANDONED', publish_abandoned_at = UTC_TIMESTAMP(3),
    publish_last_error_code = 'STAGING_OBJECT_UNAVAILABLE', publish_lease_token = NULL, publish_lease_until = NULL
WHERE staging_object_key IS NULL AND ready_at IS NULL AND created_at < ?
  AND publication_status IN ('PENDING','PUBLISHING')
  AND (publish_lease_until IS NULL OR publish_lease_until < UTC_TIMESTAMP(3))
LIMIT %d`, limit)
	result, err := repository.database.ExecContext(ctx, query, before)
	if err != nil {
		return 0, fmt.Errorf("sweep unrecoverable bundle publications: %w", err)
	}
	return result.RowsAffected()
}

func (repository *SQLBundleRepository) FindBundle(ctx context.Context, tenantID, bundleID string) (BundleMetadata, error) {
	if repository == nil || repository.database == nil || !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(bundleID) {
		return BundleMetadata{}, ErrBundleNotFound
	}
	metadata, err := scanBundleMetadata(repository.database.QueryRowContext(ctx, `
SELECT bundle.external_id, bundle.sha256, bundle.size_bytes, bundle.case_count,
       bundle.manifest_version, bundle.created_at
FROM t_external_bundle AS bundle
JOIN t_external_tenant AS tenant ON tenant.id = bundle.tenant_id
WHERE tenant.external_id = ? AND tenant.status = 'ACTIVE'
  AND bundle.external_id = ? AND bundle.publication_status = 'READY'
  AND bundle.ready_at IS NOT NULL AND bundle.deleted_at IS NULL
LIMIT 1`, tenantID, bundleID))
	if errors.Is(err, sql.ErrNoRows) {
		return BundleMetadata{}, ErrBundleNotFound
	}
	if err != nil {
		return BundleMetadata{}, fmt.Errorf("find tenant bundle: %w", err)
	}
	return metadata, nil
}

func findBundleByDigest(ctx context.Context, transaction *sql.Tx, tenantInternalID uint64, digest [sha256.Size]byte) (BundleMetadata, BundlePublicationStatus, string, bool, error) {
	metadata, status, stagingKey, err := scanBundleMetadataWithPublication(transaction.QueryRowContext(ctx, `
SELECT external_id, sha256, size_bytes, case_count, manifest_version, created_at,
       publication_status, staging_object_key
FROM t_external_bundle
WHERE tenant_id = ? AND sha256 = ? AND deleted_at IS NULL
LIMIT 1 FOR UPDATE`, tenantInternalID, digest[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return BundleMetadata{}, "", "", false, nil
	}
	if err != nil {
		return BundleMetadata{}, "", "", false, fmt.Errorf("find immutable bundle by digest: %w", err)
	}
	return metadata, status, stagingKey, true, nil
}

func scanBundleMetadataWithPublication(row rowScanner) (BundleMetadata, BundlePublicationStatus, string, error) {
	var metadata BundleMetadata
	var digest []byte
	var status string
	var stagingKey sql.NullString
	if err := row.Scan(&metadata.BundleID, &digest, &metadata.SizeBytes, &metadata.CaseCount, &metadata.ManifestVersion, &metadata.CreatedAt, &status, &stagingKey); err != nil {
		return BundleMetadata{}, "", "", err
	}
	if len(digest) != sha256.Size {
		return BundleMetadata{}, "", "", fmt.Errorf("stored bundle digest is invalid")
	}
	metadata.SHA256 = hex.EncodeToString(digest)
	metadata.CreatedAt = metadata.CreatedAt.UTC()
	if !validBundleMetadata(metadata) {
		return BundleMetadata{}, "", "", fmt.Errorf("stored bundle metadata is invalid")
	}
	publicationStatus := BundlePublicationStatus(status)
	if !validBundlePublicationStatus(publicationStatus) {
		return BundleMetadata{}, "", "", fmt.Errorf("stored bundle publication status is invalid")
	}
	return metadata, publicationStatus, stagingKey.String, nil
}

func scanBundlePublicationClaim(row rowScanner) (BundlePublicationClaim, error) {
	var claim BundlePublicationClaim
	var digest []byte
	if err := row.Scan(&claim.TenantID, &claim.BundleID, &claim.ObjectKey, &claim.StagingKey, &digest, &claim.SizeBytes, &claim.LeaseToken, &claim.AttemptCount); err != nil {
		return BundlePublicationClaim{}, err
	}
	if len(digest) != sha256.Size {
		return BundlePublicationClaim{}, fmt.Errorf("stored bundle publication digest is invalid")
	}
	copy(claim.RequestHash[:], digest)
	if !validBundlePublicationClaim(claim) {
		return BundlePublicationClaim{}, fmt.Errorf("stored bundle publication claim is invalid")
	}
	return claim, nil
}

func scanBundleMetadata(row rowScanner) (BundleMetadata, error) {
	var metadata BundleMetadata
	var digest []byte
	if err := row.Scan(&metadata.BundleID, &digest, &metadata.SizeBytes, &metadata.CaseCount, &metadata.ManifestVersion, &metadata.CreatedAt); err != nil {
		return BundleMetadata{}, err
	}
	if len(digest) != sha256.Size {
		return BundleMetadata{}, fmt.Errorf("stored bundle digest is invalid")
	}
	metadata.SHA256 = hex.EncodeToString(digest)
	metadata.CreatedAt = metadata.CreatedAt.UTC()
	if !validBundleMetadata(metadata) {
		return BundleMetadata{}, fmt.Errorf("stored bundle metadata is invalid")
	}
	return metadata, nil
}

func validBundleCommitInput(input BundleCommitInput) bool {
	digestHex := hex.EncodeToString(input.RequestHash[:])
	manifest, err := bundle.ParseManifest(input.ManifestJSON)
	return err == nil && externalIDPattern.MatchString(input.TenantID) && externalIDPattern.MatchString(input.Metadata.BundleID) &&
		input.Metadata.SHA256 == digestHex && input.ObjectKey == path.Join("external", input.TenantID, "sha256", digestHex+".zip") &&
		validStagingObjectKey(input.TenantID, input.StagingObjectKey, digestHex) &&
		input.Metadata.CaseCount == len(manifest.Cases) && input.Metadata.ManifestVersion == manifest.SchemaVersion && validBundleMetadata(input.Metadata) &&
		input.TimeLimitMillis == manifest.Limits.TimeLimitMillis && input.MemoryLimitMiB == manifest.Limits.MemoryLimitMiB &&
		input.IdempotencyExpiresAt.After(input.Metadata.CreatedAt)
}

func bundleWithinTenantPolicy(input BundleCommitInput, policy TenantPolicy) bool {
	return input.TimeLimitMillis > 0 && input.MemoryLimitMiB > 0 &&
		input.TimeLimitMillis <= policy.MaxTimeLimitMillis && input.MemoryLimitMiB <= policy.MaxMemoryLimitMiB
}

func validStagingObjectKey(tenantID, key, digestHex string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 5 && parts[0] == "external" && parts[1] == tenantID && parts[2] == "staging" &&
		externalIDPattern.MatchString(parts[3]) && parts[4] == digestHex+".zip"
}

func validBundlePublicationClaim(claim BundlePublicationClaim) bool {
	digestHex := hex.EncodeToString(claim.RequestHash[:])
	return externalIDPattern.MatchString(claim.TenantID) && externalIDPattern.MatchString(claim.BundleID) &&
		externalIDPattern.MatchString(claim.LeaseToken) && claim.SizeBytes > 0 && claim.AttemptCount > 0 &&
		claim.ObjectKey == path.Join("external", claim.TenantID, "sha256", digestHex+".zip") &&
		validStagingObjectKey(claim.TenantID, claim.StagingKey, digestHex)
}

func validFailureCode(code string) bool { return bundlePublicationFailureCodePattern.MatchString(code) }

func validBundlePublicationStatus(status BundlePublicationStatus) bool {
	return status == BundlePublicationPending || status == BundlePublicationPublishing || status == BundlePublicationReady || status == BundlePublicationAbandoned
}

func validBundleMetadata(metadata BundleMetadata) bool {
	if !externalIDPattern.MatchString(metadata.BundleID) || len(metadata.SHA256) != sha256.Size*2 || metadata.SizeBytes <= 0 || metadata.CaseCount <= 0 || metadata.ManifestVersion <= 0 || metadata.CreatedAt.IsZero() {
		return false
	}
	_, err := hex.DecodeString(metadata.SHA256)
	return err == nil
}

func statusForBundleCommit(existing bool) int {
	if existing {
		return 200
	}
	return 201
}
