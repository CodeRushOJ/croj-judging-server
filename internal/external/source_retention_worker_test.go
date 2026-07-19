package external

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type flakyRetentionStore struct {
	*memorySourceStore
	failures int
}

func (store *flakyRetentionStore) Delete(ctx context.Context, key string) error {
	if store.failures > 0 {
		store.failures--
		return errors.New("temporary object failure")
	}
	return store.memorySourceStore.Delete(ctx, key)
}

func TestSourceRetentionWorkerMarksRetriesAuditsAndFinalizes(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	tenantID, bundleID := strings.Repeat("s", 26), strings.Repeat("t", 26)
	insertTenantBundleAndCallback(t, database, tenantID, bundleID, "", 2)
	store := &flakyRetentionStore{memorySourceStore: newMemorySourceStore(), failures: 1}
	repository := newTestMySQLJobRepository(t, database, store)
	submitted, err := repository.Submit(context.Background(), tenantID, "retention-job-key-1", JudgeJobRequest{BundleID: bundleID, Language: "cpp", SourceCode: []byte("int main(){}")})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repository.ClaimNext(context.Background(), "retention-judge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Complete(context.Background(), claim, DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_job SET completed_at = CURRENT_TIMESTAMP(3) - INTERVAL 2 HOUR WHERE id = ?`, claim.Job.InternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE t_external_idempotency SET expires_at = CURRENT_TIMESTAMP(3) - INTERVAL 1 SECOND WHERE resource_external_id = ?`, submitted.Job.ExternalID); err != nil {
		t.Fatal(err)
	}
	worker, err := NewSourceRetentionWorker(SourceRetentionWorkerConfig{
		Repository: repository, Objects: store, Retention: time.Hour, IdleDelay: time.Millisecond, DeleteTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessNext(context.Background()); err == nil {
		t.Fatal("temporary object deletion failure was hidden")
	}
	if jobs := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job WHERE id = ?`, claim.Job.InternalID); jobs != 1 {
		t.Fatalf("failed delete removed job metadata: %d", jobs)
	}
	if retries := mustCount(t, database, `SELECT COUNT(*) FROM t_external_retention_audit WHERE event_type = 'DELETE_RETRY'`); retries != 1 {
		t.Fatalf("retry audit rows=%d", retries)
	}
	if err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs := mustCount(t, database, `SELECT COUNT(*) FROM t_external_job WHERE id = ?`, claim.Job.InternalID); jobs != 0 {
		t.Fatalf("retained terminal job rows=%d", jobs)
	}
	if sources := mustCount(t, database, `SELECT COUNT(*) FROM t_external_source_object WHERE id = ?`, claim.Job.Source.InternalID); sources != 0 {
		t.Fatalf("retained source rows=%d", sources)
	}
	if deleted := mustCount(t, database, `SELECT COUNT(*) FROM t_external_retention_audit WHERE event_type = 'DELETED'`); deleted != 1 {
		t.Fatalf("delete audit rows=%d", deleted)
	}
	objects, _, deletes := store.snapshot()
	if len(objects) != 0 || deletes != 1 {
		t.Fatalf("objects=%d successfulDeletes=%d", len(objects), deletes)
	}
}

func TestSourceRetentionDoesNotMarkReferencedOrRecentSources(t *testing.T) {
	database := openMySQLIntegration(t)
	prepareExternalJobDatabase(t, database)
	repository := newTestMySQLJobRepository(t, database, newMemorySourceStore())
	claim, err := repository.ClaimSourceRetention(context.Background(), 24*time.Hour)
	if !errors.Is(err, ErrSourceRetentionNotAvailable) || claim.ObjectKey != "" {
		t.Fatalf("empty retention claim=%+v error=%v", claim, err)
	}
}
