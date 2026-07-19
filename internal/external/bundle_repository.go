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

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

const bundleUploadOperationScope = "bundle-upload"

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
	err := repository.database.QueryRowContext(ctx, `
SELECT idempotency.request_hash, idempotency.response_json
FROM t_external_idempotency AS idempotency
JOIN t_external_tenant AS tenant ON tenant.id = idempotency.tenant_id
WHERE tenant.external_id = ? AND tenant.status = 'ACTIVE'
  AND idempotency.operation_scope = ? AND idempotency.key_digest = ?
  AND idempotency.expires_at > UTC_TIMESTAMP(3)
LIMIT 1`, tenantID, bundleUploadOperationScope, keyDigest[:]).Scan(&requestHash, &responseJSON)
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
	return BundleUploadLookup{Found: true, RequestHash: digest, Metadata: metadata}, nil
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
	if err := transaction.QueryRowContext(ctx, `
SELECT id FROM t_external_tenant
WHERE external_id = ? AND status = 'ACTIVE'
FOR UPDATE`, input.TenantID).Scan(&tenantInternalID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BundleCommitResult{}, ErrBundleNotFound
		}
		return BundleCommitResult{}, fmt.Errorf("lock bundle tenant: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
DELETE FROM t_external_idempotency
WHERE tenant_id = ? AND operation_scope = ? AND key_digest = ? AND expires_at <= UTC_TIMESTAMP(3)`,
		tenantInternalID, bundleUploadOperationScope, input.IdempotencyDigest[:]); err != nil {
		return BundleCommitResult{}, fmt.Errorf("remove expired bundle idempotency: %w", err)
	}

	var storedRequestHash []byte
	var storedResponse []byte
	err = transaction.QueryRowContext(ctx, `
SELECT request_hash, response_json
FROM t_external_idempotency
WHERE tenant_id = ? AND operation_scope = ? AND key_digest = ?
FOR UPDATE`, tenantInternalID, bundleUploadOperationScope, input.IdempotencyDigest[:]).Scan(&storedRequestHash, &storedResponse)
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
		if err := transaction.Commit(); err != nil {
			return BundleCommitResult{}, fmt.Errorf("commit bundle replay: %w", err)
		}
		return BundleCommitResult{Metadata: metadata, Replay: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BundleCommitResult{}, fmt.Errorf("read bundle idempotency under lock: %w", err)
	}

	metadata, found, err := findBundleByDigest(ctx, transaction, tenantInternalID, input.RequestHash)
	if err != nil {
		return BundleCommitResult{}, err
	}
	if !found {
		metadata = input.Metadata
		if _, err := transaction.ExecContext(ctx, `
INSERT INTO t_external_bundle(
    external_id, tenant_id, sha256, object_key, size_bytes,
    case_count, manifest_version, manifest_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, metadata.BundleID, tenantInternalID, input.RequestHash[:], input.ObjectKey,
			metadata.SizeBytes, metadata.CaseCount, metadata.ManifestVersion, input.ManifestJSON, metadata.CreatedAt); err != nil {
			return BundleCommitResult{}, fmt.Errorf("insert immutable bundle metadata: %w", err)
		}
	} else {
		if _, err := transaction.ExecContext(ctx, `
UPDATE t_external_bundle SET delete_marked_at = NULL
WHERE tenant_id = ? AND external_id = ? AND deleted_at IS NULL`, tenantInternalID, metadata.BundleID); err != nil {
			return BundleCommitResult{}, fmt.Errorf("retain existing immutable bundle: %w", err)
		}
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
	return BundleCommitResult{Metadata: metadata, Replay: found}, nil
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
  AND bundle.external_id = ? AND bundle.deleted_at IS NULL
LIMIT 1`, tenantID, bundleID))
	if errors.Is(err, sql.ErrNoRows) {
		return BundleMetadata{}, ErrBundleNotFound
	}
	if err != nil {
		return BundleMetadata{}, fmt.Errorf("find tenant bundle: %w", err)
	}
	return metadata, nil
}

func findBundleByDigest(ctx context.Context, transaction *sql.Tx, tenantInternalID uint64, digest [sha256.Size]byte) (BundleMetadata, bool, error) {
	metadata, err := scanBundleMetadata(transaction.QueryRowContext(ctx, `
SELECT external_id, sha256, size_bytes, case_count, manifest_version, created_at
FROM t_external_bundle
WHERE tenant_id = ? AND sha256 = ? AND deleted_at IS NULL
LIMIT 1 FOR UPDATE`, tenantInternalID, digest[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return BundleMetadata{}, false, nil
	}
	if err != nil {
		return BundleMetadata{}, false, fmt.Errorf("find immutable bundle by digest: %w", err)
	}
	return metadata, true, nil
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
		input.Metadata.CaseCount == len(manifest.Cases) && input.Metadata.ManifestVersion == manifest.SchemaVersion && validBundleMetadata(input.Metadata) &&
		input.IdempotencyExpiresAt.After(input.Metadata.CreatedAt)
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
