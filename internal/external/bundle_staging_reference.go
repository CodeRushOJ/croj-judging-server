package external

import (
	"context"
	"fmt"
)

// IsBundleStagingReferenced is deliberately a fresh autocommit read. The
// staging collector invokes it on both sides of deletion; it never holds a DB
// transaction while waiting for object storage.
func (repository *SQLBundleRepository) IsBundleStagingReferenced(ctx context.Context, objectKey string) (bool, error) {
	if repository == nil || repository.database == nil || !validBundleStagingGCKey(objectKey) {
		return false, fmt.Errorf("bundle staging reference lookup is invalid")
	}
	var referenced int
	if err := repository.database.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM t_external_bundle
WHERE staging_object_key = ? AND publication_status IN ('PENDING','PUBLISHING')
  AND deleted_at IS NULL)`, objectKey).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check bundle staging reference: %w", err)
	}
	return referenced == 1, nil
}
