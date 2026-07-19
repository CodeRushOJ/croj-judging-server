package external

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
)

// BundleObjectStore publishes a fully validated local file. Implementations
// must not expose a readable object until the complete upload succeeds.
type BundleObjectStore interface {
	Publish(context.Context, string, string, int64, [sha256.Size]byte) error
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

func (store *MinIOBundleObjectStore) Publish(ctx context.Context, key, filename string, size int64, digest [sha256.Size]byte) error {
	if store == nil || store.client == nil || store.bucket == "" || key == "" || size <= 0 {
		return fmt.Errorf("bundle object publication is not configured")
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
		return fmt.Errorf("publish immutable bundle object: %w", err)
	}
	if upload.Size != size {
		return fmt.Errorf("publish immutable bundle object: committed size mismatch")
	}
	return nil
}
