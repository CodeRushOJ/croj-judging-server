package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
)

const maximumEncryptedSourceObjectBytes = 64 << 20

type MinIOSourceObjectStore struct {
	client *minio.Client
	bucket string
}

func NewMinIOSourceObjectStore(client *minio.Client, bucket string) (*MinIOSourceObjectStore, error) {
	bucket = strings.TrimSpace(bucket)
	if client == nil || bucket == "" || strings.ContainsAny(bucket, "/\\") {
		return nil, fmt.Errorf("source object client and bucket are required")
	}
	return &MinIOSourceObjectStore{client: client, bucket: bucket}, nil
}

func (store *MinIOSourceObjectStore) Create(ctx context.Context, key string, ciphertext []byte) error {
	if store == nil || store.client == nil || !validSourceObjectKey(key) || len(ciphertext) == 0 || len(ciphertext) > maximumEncryptedSourceObjectBytes {
		return fmt.Errorf("encrypted source object create request is invalid")
	}
	options := minio.PutObjectOptions{
		ContentType:      "application/octet-stream",
		DisableMultipart: true,
		UserMetadata:     map[string]string{"coderushoj-source-encryption": "aes-256-gcm"},
	}
	options.SetMatchETagExcept("*")
	upload, err := store.client.PutObject(ctx, store.bucket, key, bytes.NewReader(ciphertext), int64(len(ciphertext)), options)
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.Code == "PreconditionFailed" || response.StatusCode == 412 {
			return ErrSourceObjectExists
		}
		return fmt.Errorf("encrypted source object create failed")
	}
	if upload.Size != int64(len(ciphertext)) {
		return fmt.Errorf("encrypted source object committed size mismatch")
	}
	return nil
}

func (store *MinIOSourceObjectStore) Get(ctx context.Context, key string, maximumBytes int64) ([]byte, error) {
	if store == nil || store.client == nil || !validSourceObjectKey(key) || maximumBytes <= 0 || maximumBytes > maximumEncryptedSourceObjectBytes {
		return nil, fmt.Errorf("%w: encrypted source object read request is invalid", ErrSourceEncryption)
	}
	object, err := store.client.GetObject(ctx, store.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: encrypted source object is unavailable", ErrSourceEncryption)
	}
	defer object.Close()
	payload, err := io.ReadAll(io.LimitReader(object, maximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: encrypted source object read failed", ErrSourceEncryption)
	}
	if int64(len(payload)) > maximumBytes {
		clear(payload)
		return nil, fmt.Errorf("%w: encrypted source object exceeds metadata size", ErrSourceEncryption)
	}
	return payload, nil
}

func (store *MinIOSourceObjectStore) Delete(ctx context.Context, key string) error {
	if store == nil || store.client == nil || !validSourceObjectKey(key) {
		return fmt.Errorf("encrypted source object delete request is invalid")
	}
	if err := store.client.RemoveObject(ctx, store.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("encrypted source object delete failed")
	}
	return nil
}

func validSourceObjectKey(key string) bool {
	if key == "" || path.Clean(key) != key || strings.Contains(key, "\\") {
		return false
	}
	segments := strings.Split(key, "/")
	return len(segments) == 4 && segments[0] == "external" &&
		externalIDPattern.MatchString(segments[1]) && segments[2] == "sources" &&
		strings.HasSuffix(segments[3], ".bin") &&
		externalIDPattern.MatchString(strings.TrimSuffix(segments[3], ".bin"))
}
