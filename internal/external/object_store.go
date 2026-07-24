package external

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// BundleObjectStore durably stages validated bytes and promotes exactly one
// leased publication to its immutable final content address.
type BundleObjectStore interface {
	Stage(context.Context, string, string, int64, [sha256.Size]byte) error
	Promote(context.Context, string, string, int64, [sha256.Size]byte) error
	Discard(context.Context, string) error
}

// BundleStagingObject is the content-opaque metadata needed to age out an
// upload that never obtained a durable database reference.
type BundleStagingObject struct {
	Key          string
	LastModified time.Time
}

type BundleStagingObjectStore interface {
	// ListStaging scans at most limit object keys strictly after the opaque
	// continuation. A non-empty returned continuation resumes the bounded scan.
	ListStaging(context.Context, time.Time, string, int) ([]BundleStagingObject, string, error)
	Discard(context.Context, string) error
}

type MinIOBundleObjectStore struct {
	client *minio.Client
	bucket string
}

func NewMinIOBundleObjectStore(client *minio.Client, bucket string) (*MinIOBundleObjectStore, error) {
	if client == nil || strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("bundle object client and bucket are required")
	}
	return &MinIOBundleObjectStore{client: client, bucket: strings.TrimSpace(bucket)}, nil
}

func (store *MinIOBundleObjectStore) Stage(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	if store == nil || store.client == nil || store.bucket == "" || key == "" || size <= 0 {
		return fmt.Errorf("bundle object staging is not configured")
	}
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open staged bundle: %w", err)
	}
	defer file.Close()
	upload, err := store.client.PutObject(ctx, store.bucket, key, file, size, minio.PutObjectOptions{
		ContentType:  "application/zip",
		UserMetadata: map[string]string{"sha256": fmt.Sprintf("%x", digest[:])},
	})
	if err != nil {
		return fmt.Errorf("stage immutable bundle object: %w", err)
	}
	if upload.Size != size {
		return fmt.Errorf("stage immutable bundle object: committed size mismatch")
	}
	info, err := store.client.StatObject(ctx, store.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("verify staged immutable bundle object: %w", err)
	}
	return verifyBundleObject(info, size, digest)
}

func (store *MinIOBundleObjectStore) Promote(ctx context.Context, stagingKey, finalKey string, size int64, digest [sha256.Size]byte) error {
	if store == nil || store.client == nil || store.bucket == "" || stagingKey == "" || finalKey == "" || size <= 0 {
		return fmt.Errorf("bundle object promotion is not configured")
	}
	if info, found, err := store.stat(ctx, finalKey); err != nil {
		return err
	} else if found {
		return verifyBundleObject(info, size, digest)
	}
	staged, found, err := store.stat(ctx, stagingKey)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("staged immutable bundle object is missing")
	}
	if err := verifyBundleObject(staged, size, digest); err != nil {
		return err
	}
	if _, err := store.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: store.bucket, Object: finalKey, ReplaceMetadata: true, ContentType: "application/zip", UserMetadata: map[string]string{"sha256": hex.EncodeToString(digest[:])}},
		minio.CopySrcOptions{Bucket: store.bucket, Object: stagingKey}); err != nil {
		return fmt.Errorf("promote immutable bundle object: %w", err)
	}
	published, found, err := store.stat(ctx, finalKey)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("promoted immutable bundle object is missing")
	}
	return verifyBundleObject(published, size, digest)
}

func (store *MinIOBundleObjectStore) Discard(ctx context.Context, key string) error {
	if store == nil || store.client == nil || store.bucket == "" || key == "" {
		return fmt.Errorf("bundle object discard is not configured")
	}
	if err := store.client.RemoveObject(ctx, store.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("discard staged immutable bundle object: %w", err)
	}
	return nil
}

func (store *MinIOBundleObjectStore) ListStaging(ctx context.Context, before time.Time, after string, limit int) ([]BundleStagingObject, string, error) {
	if store == nil || store.client == nil || store.bucket == "" || before.IsZero() || limit < 1 || limit > 1000 || after != "" && !validBundleStagingContinuation(after) {
		return nil, "", fmt.Errorf("bundle staging list is not configured")
	}
	listContext, cancel := context.WithCancel(ctx)
	defer cancel()
	objects := make([]BundleStagingObject, 0, limit)
	listed := store.client.ListObjects(listContext, store.bucket, minio.ListObjectsOptions{
		Prefix: "external-staging/", Recursive: true, StartAfter: after, MaxKeys: limit,
	})
	scanned := 0
	lastScanned := ""
	for object := range listed {
		if object.Err != nil {
			return nil, "", fmt.Errorf("list staged immutable bundle objects: %w", object.Err)
		}
		scanned++
		lastScanned = object.Key
		if !validBundleStagingGCKey(object.Key) || object.LastModified.IsZero() || !object.LastModified.Before(before) {
			if scanned < limit {
				continue
			}
		} else {
			objects = append(objects, BundleStagingObject{Key: object.Key, LastModified: object.LastModified.UTC()})
		}
		if scanned == limit {
			cancel()
			for range listed {
			}
			break
		}
	}
	if scanned < limit {
		lastScanned = ""
	}
	return objects, lastScanned, nil
}

func validBundleStagingContinuation(value string) bool {
	if len(value) <= len("external-staging/") || len(value) > 1024 || !strings.HasPrefix(value, "external-staging/") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validBundleStagingGCKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "external-staging" || !externalIDPattern.MatchString(parts[1]) ||
		!externalIDPattern.MatchString(parts[2]) || !strings.HasSuffix(parts[3], ".zip") {
		return false
	}
	digest := strings.TrimSuffix(parts[3], ".zip")
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

func (store *MinIOBundleObjectStore) stat(ctx context.Context, key string) (minio.ObjectInfo, bool, error) {
	info, err := store.client.StatObject(ctx, store.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return info, true, nil
	}
	response := minio.ToErrorResponse(err)
	if response.StatusCode == http.StatusNotFound || response.Code == minio.NoSuchKey {
		return minio.ObjectInfo{}, false, nil
	}
	return minio.ObjectInfo{}, false, fmt.Errorf("stat immutable bundle object: %w", err)
}

func verifyBundleObject(info minio.ObjectInfo, size int64, digest [sha256.Size]byte) error {
	wantDigest := hex.EncodeToString(digest[:])
	actualDigest := info.Metadata.Get("X-Amz-Meta-Sha256")
	if actualDigest == "" {
		actualDigest = info.UserMetadata["sha256"]
	}
	if info.Size != size || !strings.EqualFold(actualDigest, wantDigest) {
		return fmt.Errorf("immutable bundle object size or SHA-256 metadata mismatch")
	}
	return nil
}
