package bundle

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinIOConfig struct {
	Endpoint  string
	Bucket    string
	Region    string
	UseTLS    bool
	AccessKey string
	SecretKey string
}

type MinIOStore struct {
	client *minio.Client
	bucket string
}

func NewMinIOStore(config MinIOConfig) (*MinIOStore, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	parsed, err := url.Parse("//" + endpoint)
	if err != nil || endpoint == "" || parsed.Host != endpoint || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, fmt.Errorf("bundle object endpoint must be host[:port] without scheme or path")
	}
	if strings.TrimSpace(config.Bucket) == "" || strings.TrimSpace(config.Region) == "" || config.AccessKey == "" || config.SecretKey == "" {
		return nil, fmt.Errorf("bundle object bucket, region, access key, and secret key are required")
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure: config.UseTLS,
		Region: strings.TrimSpace(config.Region),
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3-compatible bundle client: %w", err)
	}
	return &MinIOStore{client: client, bucket: strings.TrimSpace(config.Bucket)}, nil
}

func (store *MinIOStore) Open(ctx context.Context, key string) (Object, error) {
	object, err := store.client.GetObject(ctx, store.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return Object{}, fmt.Errorf("get bundle object: %w", err)
	}
	info, err := object.Stat()
	if err != nil {
		_ = object.Close()
		response := minio.ToErrorResponse(err)
		switch response.Code {
		case "NoSuchKey", "NoSuchObject", "NoSuchBucket", "NotFound":
			return Object{}, Invalid(fmt.Errorf("bundle object does not exist: %w", err))
		}
		return Object{}, fmt.Errorf("stat bundle object: %w", err)
	}
	return Object{Body: object, Size: info.Size}, nil
}
