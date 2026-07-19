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
	Ready       bool
	RequestHash [sha256.Size]byte
	Metadata    BundleMetadata
}

type BundleCommitInput struct {
	TenantID             string
	IdempotencyDigest    [sha256.Size]byte
	RequestHash          [sha256.Size]byte
	ObjectKey            string
	ManifestJSON         []byte
	Metadata             BundleMetadata
	IdempotencyExpiresAt time.Time
}

type BundleCommitResult struct {
	Metadata BundleMetadata
	Replay   bool
	Ready    bool
}

type BundleRepository interface {
	FindBundleUpload(context.Context, string, [sha256.Size]byte) (BundleUploadLookup, error)
	CommitBundleUpload(context.Context, BundleCommitInput) (BundleCommitResult, error)
	MarkBundleReady(context.Context, string, string, [sha256.Size]byte) error
	FindBundle(context.Context, string, string) (BundleMetadata, error)
}

type BundleServiceConfig struct {
	TempDir           string
	MaxUploadBytes    int64
	ArchiveLimits     bundle.ArchiveLimits
	IdempotencyTTL    time.Duration
	IdempotencyPepper []byte
	Random            io.Reader
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
	if config.MaxUploadBytes <= 0 || config.IdempotencyTTL <= 0 {
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
	config.IdempotencyPepper = append([]byte(nil), config.IdempotencyPepper...)
	return &BundleService{repository: repository, store: store, config: config, now: time.Now}, nil
}

func (service *BundleService) Upload(ctx context.Context, tenantID, idempotencyKey string, source io.Reader) (BundleMetadata, bool, error) {
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
	lookup, err := service.repository.FindBundleUpload(ctx, tenantID, idempotencyDigest)
	if err != nil {
		return BundleMetadata{}, false, fmt.Errorf("find bundle upload idempotency: %w", err)
	}
	if lookup.Found {
		if lookup.RequestHash != requestHash {
			return BundleMetadata{}, false, ErrIdempotencyConflict
		}
		if !lookup.Ready {
			objectKey := bundleObjectKey(tenantID, requestHash)
			if err := service.store.Publish(ctx, objectKey, filename, written, requestHash); err != nil {
				return BundleMetadata{}, false, fmt.Errorf("reconcile immutable bundle object: %w", err)
			}
			if err := service.repository.MarkBundleReady(ctx, tenantID, lookup.Metadata.BundleID, requestHash); err != nil {
				return BundleMetadata{}, false, fmt.Errorf("mark reconciled immutable bundle ready: %w", err)
			}
		}
		return lookup.Metadata, true, nil
	}

	digestHex := hex.EncodeToString(requestHash[:])
	objectKey := bundleObjectKey(tenantID, requestHash)
	bundleID, err := generateExternalID(service.config.Random)
	if err != nil {
		return BundleMetadata{}, false, err
	}
	now := service.now().UTC()
	result, err := service.repository.CommitBundleUpload(ctx, BundleCommitInput{
		TenantID: tenantID, IdempotencyDigest: idempotencyDigest, RequestHash: requestHash,
		ObjectKey: objectKey, ManifestJSON: manifestJSON,
		Metadata:             BundleMetadata{BundleID: bundleID, SHA256: digestHex, SizeBytes: written, CaseCount: len(manifest.Cases), ManifestVersion: manifest.SchemaVersion, CreatedAt: now},
		IdempotencyExpiresAt: now.Add(service.config.IdempotencyTTL),
	})
	if err != nil {
		return BundleMetadata{}, false, err
	}
	if !result.Ready {
		// Ownership remains hidden until the complete object is atomically
		// published. A failed publish is safely repaired by the same idempotent
		// upload without ever exposing pending metadata or an unowned object.
		if err := service.store.Publish(ctx, objectKey, filename, written, requestHash); err != nil {
			return BundleMetadata{}, false, fmt.Errorf("publish owned immutable bundle object: %w", err)
		}
		if err := service.repository.MarkBundleReady(ctx, tenantID, result.Metadata.BundleID, requestHash); err != nil {
			return BundleMetadata{}, false, fmt.Errorf("mark immutable bundle ready: %w", err)
		}
	}
	return result.Metadata, result.Replay, nil
}

func bundleObjectKey(tenantID string, digest [sha256.Size]byte) string {
	return path.Join("external", tenantID, "sha256", hex.EncodeToString(digest[:])+".zip")
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
