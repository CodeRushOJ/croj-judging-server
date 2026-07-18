package bundle

import (
	"context"
	"errors"
	"fmt"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type invalidError struct{ cause error }

func (err invalidError) Error() string { return err.cause.Error() }
func (err invalidError) Unwrap() error { return err.cause }

func Invalid(err error) error {
	if err == nil || IsInvalid(err) {
		return err
	}
	return invalidError{cause: err}
}

func IsInvalid(err error) bool {
	var target invalidError
	return errors.As(err, &target)
}

type Provider struct {
	cache  *Cache
	limits ArchiveLimits
}

type ArtifactReader interface {
	Manifest() Manifest
	ReadCase(Case) (string, string, error)
	Close() error
}

func NewProvider(cache *Cache, limits ArchiveLimits) *Provider {
	return &Provider{cache: cache, limits: limits}
}

func (provider *Provider) Open(ctx context.Context, metadata *model.TestBundle) (ArtifactReader, error) {
	if provider == nil || provider.cache == nil || metadata == nil || metadata.ProblemVersionID <= 0 {
		return nil, Invalid(fmt.Errorf("test bundle metadata is missing or invalid"))
	}
	path, err := provider.cache.Resolve(ctx, Metadata{
		ObjectKey: metadata.ObjectKey,
		SHA256:    metadata.SHA256,
		SizeBytes: metadata.SizeBytes,
	})
	if err != nil {
		return nil, err
	}
	artifact, err := OpenArchive(path, metadata.ManifestJSON, provider.limits)
	if err != nil {
		return nil, Invalid(err)
	}
	return artifact, nil
}
