package external

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
)

var (
	ErrInvalidBundle       = errors.New("invalid immutable bundle")
	ErrBundleTooLarge      = errors.New("immutable bundle exceeds upload limit")
	ErrBundleNotFound      = errors.New("immutable bundle was not found")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different content")
	ErrInvalidIdempotency  = errors.New("idempotency key is invalid")
	ErrBundlePublishing    = errors.New("immutable bundle publication is in progress")
	ErrBundleAbandoned     = errors.New("immutable bundle publication was abandoned")
)

const maxExternalBundleCasesV1 = 256

type BundleMetadata struct {
	BundleID        string    `json:"bundleId"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"sizeBytes"`
	CaseCount       int       `json:"caseCount"`
	ManifestVersion int       `json:"manifestVersion"`
	CreatedAt       time.Time `json:"createdAt"`
}

type BundleUploadLookup struct {
	Found       bool
	Status      BundlePublicationStatus
	RequestHash [sha256.Size]byte
	StagingKey  string
	Metadata    BundleMetadata
}

type BundlePublicationStatus string

const (
	BundlePublicationPending    BundlePublicationStatus = "PENDING"
	BundlePublicationPublishing BundlePublicationStatus = "PUBLISHING"
	BundlePublicationReady      BundlePublicationStatus = "READY"
	BundlePublicationAbandoned  BundlePublicationStatus = "ABANDONED"
)

type BundleCommitInput struct {
	TenantID             string
	IdempotencyDigest    [sha256.Size]byte
	RequestHash          [sha256.Size]byte
	ObjectKey            string
	StagingObjectKey     string
	ManifestJSON         []byte
	TimeLimitMillis      int
	MemoryLimitMiB       int
	Metadata             BundleMetadata
	IdempotencyExpiresAt time.Time
}

type BundleCommitResult struct {
	Metadata   BundleMetadata
	Replay     bool
	Status     BundlePublicationStatus
	StagingKey string
}

type BundlePublicationClaim struct {
	TenantID     string
	BundleID     string
	ObjectKey    string
	StagingKey   string
	RequestHash  [sha256.Size]byte
	SizeBytes    int64
	LeaseToken   string
	AttemptCount int
}

type BundleRepository interface {
	FindBundleUpload(context.Context, string, [sha256.Size]byte) (BundleUploadLookup, error)
	CommitBundleUpload(context.Context, BundleCommitInput) (BundleCommitResult, error)
	ClaimBundlePublication(context.Context, string, string, string, time.Time, time.Time) (BundlePublicationClaim, bool, error)
	ClaimNextBundlePublication(context.Context, string, time.Time, time.Time) (BundlePublicationClaim, bool, error)
	CompleteBundlePublication(context.Context, BundlePublicationClaim, time.Time) error
	FailBundlePublication(context.Context, BundlePublicationClaim, string, time.Time, int) (bool, error)
	SweepUnrecoverableBundlePublications(context.Context, time.Time, int) (int64, error)
	FindBundle(context.Context, string, string) (BundleMetadata, error)
}

type BundleUploadAdmission func(context.Context, int64) error

type BundleServiceConfig struct {
	TempDir             string
	MaxUploadBytes      int64
	ArchiveLimits       bundle.ArchiveLimits
	MaxTimeLimitMillis  int
	MaxMemoryLimitMiB   int
	IdempotencyTTL      time.Duration
	IdempotencyPepper   []byte
	PublicationLease    time.Duration
	PublicationRetry    time.Duration
	MaxPublishAttempts  int
	PendingAbandonAfter time.Duration
	Random              io.Reader
}

type BundleService struct {
	repository BundleRepository
	store      BundleObjectStore
	config     BundleServiceConfig
	now        func() time.Time
}

func NewBundleService(repository BundleRepository, store BundleObjectStore, config BundleServiceConfig) (*BundleService, error) {
	if repository == nil || store == nil {
		return nil, fmt.Errorf("bundle repository and object store are required")
	}
	if config.MaxUploadBytes <= 0 || config.IdempotencyTTL <= 0 || config.MaxTimeLimitMillis <= 0 || config.MaxMemoryLimitMiB <= 0 {
		return nil, fmt.Errorf("bundle upload and idempotency limits must be positive")
	}
	if len(config.IdempotencyPepper) < sha256.Size {
		return nil, fmt.Errorf("idempotency pepper must contain at least 256 bits")
	}
	if err := bundle.ValidateArchiveLimits(config.ArchiveLimits); err != nil {
		return nil, err
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.TempDir == "" {
		config.TempDir = os.TempDir()
	}
	if config.PublicationLease <= 0 {
		config.PublicationLease = time.Minute
	} else if config.PublicationLease < 2*time.Second {
		return nil, fmt.Errorf("bundle publication lease must be at least two seconds")
	}
	if config.PublicationRetry <= 0 {
		config.PublicationRetry = 5 * time.Second
	}
	if config.MaxPublishAttempts <= 0 {
		config.MaxPublishAttempts = 8
	}
	if config.PendingAbandonAfter <= 0 {
		config.PendingAbandonAfter = 24 * time.Hour
	}
	config.IdempotencyPepper = append([]byte(nil), config.IdempotencyPepper...)
	return &BundleService{repository: repository, store: store, config: config, now: time.Now}, nil
}

func (service *BundleService) Upload(ctx context.Context, tenantID, idempotencyKey string, source io.Reader) (BundleMetadata, bool, error) {
	return service.UploadWithAdmission(ctx, tenantID, idempotencyKey, source, nil)
}

func (service *BundleService) UploadWithAdmission(ctx context.Context, tenantID, idempotencyKey string, source io.Reader, admit BundleUploadAdmission) (BundleMetadata, bool, error) {
	if service == nil || source == nil || !externalIDPattern.MatchString(tenantID) {
		return BundleMetadata{}, false, fmt.Errorf("%w: tenant and bundle stream are required", ErrInvalidBundle)
	}
	idempotencyDigestBytes, err := DigestIdempotencyKey(idempotencyKey, service.config.IdempotencyPepper)
	if err != nil {
		return BundleMetadata{}, false, ErrInvalidIdempotency
	}
	var idempotencyDigest [sha256.Size]byte
	copy(idempotencyDigest[:], idempotencyDigestBytes)
	staged, err := os.CreateTemp(service.config.TempDir, ".external-bundle-*.zip")
	if err != nil {
		return BundleMetadata{}, false, fmt.Errorf("create bundle staging file: %w", err)
	}
	filename := staged.Name()
	defer os.Remove(filename)
	closed := false
	defer func() {
		if !closed {
			_ = staged.Close()
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(staged, hasher), io.LimitReader(&contextReader{ctx: ctx, reader: source}, service.config.MaxUploadBytes+1))
	if err != nil {
		return BundleMetadata{}, false, err
	}
	if written > service.config.MaxUploadBytes {
		return BundleMetadata{}, false, ErrBundleTooLarge
	}
	if written == 0 {
		return BundleMetadata{}, false, fmt.Errorf("%w: upload is empty", ErrInvalidBundle)
	}
	if admit != nil {
		if err := admit(ctx, written); err != nil {
			return BundleMetadata{}, false, err
		}
	}
	if err := staged.Close(); err != nil {
		return BundleMetadata{}, false, fmt.Errorf("close bundle staging file: %w", err)
	}
	closed = true
	var requestHash [sha256.Size]byte
	copy(requestHash[:], hasher.Sum(nil))

	manifest, manifestJSON, err := bundle.InspectArchive(filename, service.config.ArchiveLimits)
	if err != nil {
		return BundleMetadata{}, false, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if len(manifest.Cases) > maxExternalBundleCasesV1 {
		return BundleMetadata{}, false, fmt.Errorf("%w: manifest exceeds %d cases", ErrInvalidBundle, maxExternalBundleCasesV1)
	}
	if manifest.Limits.TimeLimitMillis > service.config.MaxTimeLimitMillis || manifest.Limits.MemoryLimitMiB > service.config.MaxMemoryLimitMiB {
		return BundleMetadata{}, false, fmt.Errorf("%w: execution limits exceed platform maximum", ErrInvalidBundle)
	}
	lookup, err := service.repository.FindBundleUpload(ctx, tenantID, idempotencyDigest)
	if err != nil {
		return BundleMetadata{}, false, fmt.Errorf("find bundle upload idempotency: %w", err)
	}
	if lookup.Found {
		if lookup.RequestHash != requestHash {
			return BundleMetadata{}, false, ErrIdempotencyConflict
		}
		switch lookup.Status {
		case BundlePublicationReady:
			return lookup.Metadata, true, nil
		}
		if !bundlePublicationNeedsFreshStaging(lookup.Status, lookup.StagingKey) {
			if err := service.publishOwnedBundle(ctx, tenantID, lookup.Metadata, requestHash); err != nil {
				return BundleMetadata{}, false, err
			}
			return lookup.Metadata, true, nil
		}
	}

	digestHex := hex.EncodeToString(requestHash[:])
	objectKey := bundleObjectKey(tenantID, requestHash)
	uploadID, err := generateExternalID(service.config.Random)
	if err != nil {
		return BundleMetadata{}, false, err
	}
	bundleID, err := generateExternalID(service.config.Random)
	if err != nil {
		return BundleMetadata{}, false, err
	}
	stagingKey := stagingBundleObjectKey(tenantID, uploadID, requestHash)
	if err := service.store.Stage(ctx, stagingKey, filename, written, requestHash); err != nil {
		return BundleMetadata{}, false, fmt.Errorf("stage immutable bundle object: %w", err)
	}
	now := service.now().UTC()
	result, err := service.repository.CommitBundleUpload(ctx, BundleCommitInput{
		TenantID: tenantID, IdempotencyDigest: idempotencyDigest, RequestHash: requestHash,
		ObjectKey: objectKey, StagingObjectKey: stagingKey, ManifestJSON: manifestJSON,
		TimeLimitMillis: manifest.Limits.TimeLimitMillis, MemoryLimitMiB: manifest.Limits.MemoryLimitMiB,
		Metadata:             BundleMetadata{BundleID: bundleID, SHA256: digestHex, SizeBytes: written, CaseCount: len(manifest.Cases), ManifestVersion: manifest.SchemaVersion, CreatedAt: now},
		IdempotencyExpiresAt: now.Add(service.config.IdempotencyTTL),
	})
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrBundleNotFound) || errors.Is(err, ErrInvalidBundle) {
			_ = service.store.Discard(context.Background(), stagingKey)
		}
		return BundleMetadata{}, false, err
	}
	if result.StagingKey != stagingKey {
		_ = service.store.Discard(context.Background(), stagingKey)
	}
	if result.Status == BundlePublicationReady {
		return result.Metadata, result.Replay, nil
	}
	if result.Status == BundlePublicationAbandoned {
		return BundleMetadata{}, false, ErrBundleAbandoned
	}
	if err := service.publishOwnedBundle(ctx, tenantID, result.Metadata, requestHash); err != nil {
		return BundleMetadata{}, false, err
	}
	return result.Metadata, result.Replay, nil
}

func (service *BundleService) publishOwnedBundle(ctx context.Context, tenantID string, metadata BundleMetadata, digest [sha256.Size]byte) error {
	leaseToken, err := generateExternalID(service.config.Random)
	if err != nil {
		return err
	}
	now := service.now().UTC()
	claim, claimed, err := service.repository.ClaimBundlePublication(ctx, tenantID, metadata.BundleID, leaseToken, now, now.Add(service.config.PublicationLease))
	if err != nil {
		return err
	}
	if !claimed {
		if _, findErr := service.repository.FindBundle(ctx, tenantID, metadata.BundleID); findErr == nil {
			return nil
		}
		return ErrBundlePublishing
	}
	if claim.RequestHash != digest {
		return fmt.Errorf("claimed bundle digest does not match upload")
	}
	promotionContext, cancelPromotion := context.WithTimeout(ctx, service.config.PublicationLease/2)
	defer cancelPromotion()
	if err := service.store.Promote(promotionContext, claim.StagingKey, claim.ObjectKey, claim.SizeBytes, claim.RequestHash); err != nil {
		nextAttempt := now.Add(service.config.PublicationRetry)
		_, _ = service.repository.FailBundlePublication(context.Background(), claim, "OBJECT_PROMOTION_FAILED", nextAttempt, service.config.MaxPublishAttempts)
		return fmt.Errorf("promote immutable bundle object: %w", err)
	}
	if err := service.repository.CompleteBundlePublication(ctx, claim, service.now().UTC()); err != nil {
		return fmt.Errorf("complete immutable bundle publication: %w", err)
	}
	_ = service.store.Discard(context.Background(), claim.StagingKey)
	return nil
}

func bundleObjectKey(tenantID string, digest [sha256.Size]byte) string {
	return path.Join("external", tenantID, "sha256", hex.EncodeToString(digest[:])+".zip")
}

func stagingBundleObjectKey(tenantID, uploadID string, digest [sha256.Size]byte) string {
	return path.Join("external", tenantID, "staging", uploadID, hex.EncodeToString(digest[:])+".zip")
}

func bundlePublicationNeedsFreshStaging(status BundlePublicationStatus, stagingKey string) bool {
	return status == BundlePublicationAbandoned || stagingKey == ""
}

func (service *BundleService) Get(ctx context.Context, tenantID, bundleID string) (BundleMetadata, error) {
	if service == nil || !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(bundleID) {
		return BundleMetadata{}, ErrBundleNotFound
	}
	return service.repository.FindBundle(ctx, tenantID, bundleID)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-reader.ctx.Done():
		return 0, reader.ctx.Err()
	default:
		return reader.reader.Read(buffer)
	}
}
