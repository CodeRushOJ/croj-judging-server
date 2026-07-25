package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type durableJobRepositoryStub struct {
	submitResult      external.SubmitJobResult
	submitError       error
	listResult        external.JobListResult
	listError         error
	getResult         external.ExternalJobRecord
	getError          error
	cancelResult      external.ExternalJobRecord
	cancelError       error
	tenantID          string
	key               string
	request           external.JudgeJobRequest
	listOptions       external.JobListOptions
	submitDeadline    time.Time
	admissionDeadline time.Time
}

func (repository *durableJobRepositoryStub) Submit(ctx context.Context, tenantID, key string, request external.JudgeJobRequest, admit func(context.Context) error) (external.SubmitJobResult, error) {
	repository.tenantID, repository.key, repository.request = tenantID, key, request
	repository.submitDeadline, _ = ctx.Deadline()
	if !repository.submitResult.Replayed && repository.submitError == nil {
		admissionContext, cancel := context.WithCancel(ctx)
		defer cancel()
		repository.admissionDeadline, _ = admissionContext.Deadline()
		if err := admit(admissionContext); err != nil {
			return external.SubmitJobResult{}, err
		}
	}
	return repository.submitResult, repository.submitError
}

func (repository *durableJobRepositoryStub) List(_ context.Context, tenantID string, options external.JobListOptions) (external.JobListResult, error) {
	repository.tenantID, repository.listOptions = tenantID, options
	return repository.listResult, repository.listError
}

func (repository *durableJobRepositoryStub) Get(_ context.Context, tenantID, _ string) (external.ExternalJobRecord, error) {
	repository.tenantID = tenantID
	return repository.getResult, repository.getError
}

func (repository *durableJobRepositoryStub) Cancel(_ context.Context, tenantID, _ string) (external.ExternalJobRecord, error) {
	repository.tenantID = tenantID
	return repository.cancelResult, repository.cancelError
}

func TestMySQLJobServiceMapsCommandsAndRedactsRepositoryInternals(t *testing.T) {
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	score, total, caseScore, caseMaximum := 100, 100, 100, 100
	record := external.ExternalJobRecord{
		InternalID: 99, ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa",
		TenantExternalID: "bbbbbbbbbbbbbbbbbbbbbbbbbb",
		BundleExternalID: "cccccccccccccccccccccccccc",
		Source: external.SourceObjectMetadata{
			ExternalID: "dddddddddddddddddddddddddd", ObjectKey: "external/secret/source.bin",
			SHA256: []byte("secret-digest"), Nonce: []byte("secret-nonce"), KeyVersion: 7,
		},
		Status: external.JobStatusSucceeded, Language: "cpp", ClientReference: "client-7",
		WorkerID: "private-worker", CreatedAt: now,
		Result: &external.DurableJobResult{
			Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: 7, MemoryBytes: 1024,
			Score: &score, TotalScore: &total,
			Cases: []external.DurableCaseResult{{CaseID: "case-1", Verdict: "ACCEPTED", TimeMillis: 7, MemoryBytes: 1024, Score: &caseScore, MaxScore: &caseMaximum}},
		},
	}
	repository := &durableJobRepositoryStub{submitResult: external.SubmitJobResult{Job: record, Replayed: true}}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	view, replayed, err := service.Submit(context.Background(), record.TenantExternalID, "idempotency-key", SubmitJobCommand{
		BundleID: record.BundleExternalID, Language: "cpp", SourceCode: "int main(){}",
		StopOnFailure: true, CallbackID: "eeeeeeeeeeeeeeeeeeeeeeeeee", ClientReference: "client-7",
	}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || view.JobID != record.ExternalID || view.Status != JobSucceeded || view.Result == nil || view.Result.Verdict != "ACCEPTED" {
		t.Fatalf("view = %+v replayed=%v", view, replayed)
	}
	if view.Result.Score == nil || *view.Result.Score != 100 || view.Result.TotalScore == nil ||
		*view.Result.TotalScore != 100 || view.Result.Cases[0].Score == nil ||
		*view.Result.Cases[0].Score != 100 {
		t.Fatalf("score view = %+v", view.Result)
	}
	if repository.request.BundleID != record.BundleExternalID || string(repository.request.SourceCode) != "int main(){}" || repository.request.CallbackID == "" {
		t.Fatalf("repository request = %+v", repository.request)
	}
	if view.StatusURL != "" {
		t.Fatalf("adapter forged transport URL = %q", view.StatusURL)
	}
}

func TestMySQLJobServiceMapsListAndPublicErrors(t *testing.T) {
	record := external.ExternalJobRecord{
		ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", TenantExternalID: "bbbbbbbbbbbbbbbbbbbbbbbbbb",
		Status: external.JobStatusQueued, CreatedAt: time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC),
	}
	repository := &durableJobRepositoryStub{listResult: external.JobListResult{Jobs: []external.ExternalJobRecord{record}, NextCursor: "signed.cursor"}}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), record.TenantExternalID, JobListQuery{Cursor: "prior.cursor", Limit: 25, Status: JobQueued})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].JobID != record.ExternalID || page.NextCursor != "signed.cursor" {
		t.Fatalf("page = %+v", page)
	}
	if repository.listOptions.Cursor != "prior.cursor" || repository.listOptions.Limit != 25 || repository.listOptions.Status != external.JobStatusQueued {
		t.Fatalf("list options = %+v", repository.listOptions)
	}

	for name, test := range map[string]struct {
		repositoryError error
		publicError     error
	}{
		"not found":   {external.ErrExternalJobNotFound, ErrJobNotFound},
		"conflict":    {external.ErrExternalJobConflict, ErrIdempotencyConflict},
		"invalid":     {external.ErrExternalJobInvalid, ErrJobInvalid},
		"quota":       {external.ErrQueuedQuotaExceeded, ErrJobQuotaExceeded},
		"unavailable": {external.ErrExternalJobUnavailable, ErrJobUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			repository.getError = test.repositoryError
			_, err := service.Get(context.Background(), record.TenantExternalID, record.ExternalID)
			if !errors.Is(err, test.publicError) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestMySQLJobServicePreservesAdmissionError(t *testing.T) {
	repository := &durableJobRepositoryStub{}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	quotaError := errors.New("quota unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err = service.Submit(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbb", "idempotency-key", SubmitJobCommand{
		BundleID: "cccccccccccccccccccccccccc", Language: "cpp", SourceCode: "int main(){}",
	}, func(admissionContext context.Context) error {
		deadline, ok := admissionContext.Deadline()
		if !ok || !deadline.Equal(repository.submitDeadline) {
			t.Fatalf("admission context deadline=%s submit deadline=%s", deadline, repository.submitDeadline)
		}
		return quotaError
	})
	if !errors.Is(err, quotaError) {
		t.Fatalf("admission error was remapped: %v", err)
	}
	if !repository.admissionDeadline.Equal(repository.submitDeadline) {
		t.Fatalf("repository admission deadline=%s submit deadline=%s", repository.admissionDeadline, repository.submitDeadline)
	}
}

func TestNewMySQLJobServiceRejectsNilRepository(t *testing.T) {
	if _, err := NewMySQLJobService(nil); err == nil {
		t.Fatal("nil repository accepted")
	}
}

func TestMySQLJobServiceValidatesAndMapsCanonicalLanguageBeforePersistence(t *testing.T) {
	repository := &durableJobRepositoryStub{submitResult: external.SubmitJobResult{Job: external.ExternalJobRecord{
		ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Status: external.JobStatusQueued, CreatedAt: time.Now(),
	}}}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Submit(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", "idempotency-key", SubmitJobCommand{
		BundleID: "cccccccccccccccccccccccccc", Language: "cpp20", SourceCode: "int main(){}",
	}, func(context.Context) error { return nil })
	if !errors.Is(err, ErrJobInvalid) {
		t.Fatalf("unsupported advertised language error = %v, want ErrJobInvalid", err)
	}
	if repository.request.Language != "" {
		t.Fatalf("unsupported language reached repository: %+v", repository.request)
	}

	repository.submitResult.Job.Language = "cpp"
	_, _, err = service.Submit(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", "another-idempotency-key", SubmitJobCommand{
		BundleID: "cccccccccccccccccccccccccc", Language: "cpp", SourceCode: "int main(){}",
	}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if repository.request.Language != "cpp" {
		t.Fatalf("sandbox language = %q, want cpp", repository.request.Language)
	}
}

func TestMySQLJobServiceExposesStableFailureCodeWithoutInternals(t *testing.T) {
	repository := &durableJobRepositoryStub{getResult: external.ExternalJobRecord{
		ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Status: external.JobStatusFailed,
		CreatedAt: time.Now(), FailureCode: "SANDBOX_UNAVAILABLE", WorkerID: "private-worker-7",
	}}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.Get(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", repository.getResult.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if view.FailureCode != "SANDBOX_UNAVAILABLE" {
		t.Fatalf("failureCode = %q", view.FailureCode)
	}
}

func TestMySQLJobServiceHidesFailureCodeOutsideFailedState(t *testing.T) {
	repository := &durableJobRepositoryStub{getResult: external.ExternalJobRecord{
		ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Status: external.JobStatusQueued,
		CreatedAt: time.Now(), FailureCode: "MIGRATION_ACCOUNTING_RECLAIM",
	}}
	service, err := NewMySQLJobService(repository)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.Get(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbb", repository.getResult.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if view.FailureCode != "" {
		t.Fatalf("queued job leaked historical failureCode = %q", view.FailureCode)
	}
}
