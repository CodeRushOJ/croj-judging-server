package external

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBundleStagingReferencesOnlyProtectActivePublicationStates(t *testing.T) {
	database := openMySQLIntegration(t)
	repository, err := NewSQLBundleRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	stagingKey := "external-staging/aaaaaaaaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbb/" +
		strings.Repeat("0", 64) + ".zip"
	for _, test := range []struct {
		status     BundlePublicationStatus
		referenced bool
	}{
		{status: BundlePublicationPending, referenced: true},
		{status: BundlePublicationPublishing, referenced: true},
		{status: BundlePublicationReady, referenced: false},
		{status: BundlePublicationAbandoned, referenced: false},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			prepareExternalJobDatabase(t, database)
			tenantID := strings.Repeat("a", 26)
			bundleID := strings.Repeat("c", 26)
			insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
			var readyAt, abandonedAt, leaseToken, leaseUntil any
			if test.status == BundlePublicationPublishing {
				leaseToken = strings.Repeat("d", 26)
				leaseUntil = time.Now().UTC().Add(time.Minute)
			}
			if test.status == BundlePublicationReady {
				readyAt = time.Now().UTC()
			}
			if test.status == BundlePublicationAbandoned {
				abandonedAt = time.Now().UTC()
			}
			if _, err := database.Exec(`
UPDATE t_external_bundle
SET staging_object_key = ?, publication_status = ?, ready_at = ?,
    publish_abandoned_at = ?, publish_lease_token = ?, publish_lease_until = ?
WHERE external_id = ?`, stagingKey, test.status, readyAt, abandonedAt, leaseToken, leaseUntil, bundleID); err != nil {
				t.Fatal(err)
			}
			referenced, err := repository.IsBundleStagingReferenced(context.Background(), stagingKey)
			if err != nil {
				t.Fatal(err)
			}
			if referenced != test.referenced {
				t.Fatalf("status %s referenced=%v want=%v", test.status, referenced, test.referenced)
			}
		})
	}
}
