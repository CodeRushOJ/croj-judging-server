package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestDurableResultPreservesCanonicalVerdictCasesAndUnits(t *testing.T) {
	score, total, firstScore, firstMax, secondScore, secondMax := 30, 100, 30, 30, 0, 70
	result, err := durableResult(service.CanonicalResult{
		Status: callback.StatusWrongAnswer, TimeUsedMillis: 17, MemoryUsedKB: 2048,
		Score: &score, TotalScore: &total,
		Cases: []service.CanonicalCaseResult{
			{CaseID: "case-1", Status: callback.StatusAccepted, TimeUsedMillis: 11, MemoryUsedKB: 1024, Score: &firstScore, MaxScore: &firstMax},
			{CaseID: "case-2", Status: callback.StatusWrongAnswer, TimeUsedMillis: 17, MemoryUsedKB: 2048, Score: &secondScore, MaxScore: &secondMax},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "WRONG_ANSWER" || result.CompileStatus != "SUCCEEDED" || result.TimeMillis != 17 || result.MemoryBytes != 2*1024*1024 || len(result.Cases) != 2 {
		t.Fatalf("durable result = %+v", result)
	}
	if result.Cases[0].Verdict != "ACCEPTED" || result.Cases[0].MemoryBytes != 1024*1024 || result.Cases[1].CaseID != "case-2" {
		t.Fatalf("durable cases = %+v", result.Cases)
	}
	if result.Score == nil || *result.Score != 30 || result.TotalScore == nil || *result.TotalScore != 100 ||
		result.Cases[0].Score == nil || *result.Cases[0].Score != 30 ||
		result.Cases[1].MaxScore == nil || *result.Cases[1].MaxScore != 70 {
		t.Fatalf("durable score = %+v", result)
	}

	compile, err := durableResult(service.CanonicalResult{Status: callback.StatusCompileError, CompileError: "redacted"})
	if err != nil || compile.CompileStatus != "FAILED" || compile.Verdict != "COMPILE_ERROR" {
		t.Fatalf("compile result=%+v error=%v", compile, err)
	}
}

func TestDurableResultRejectsInfrastructureSystemError(t *testing.T) {
	if _, err := durableResult(service.CanonicalResult{Status: callback.StatusSystemError}); err == nil {
		t.Fatal("SYSTEM_ERROR must be routed through infrastructure failure")
	}
}

func TestDurableVerdictAcceptsOutputLimitExceeded(t *testing.T) {
	if !durableVerdict(callback.StatusOutputLimitExceeded) {
		t.Fatal("OUTPUT_LIMIT_EXCEEDED must be a durable contestant verdict")
	}
}

func TestRunnerPropagatesCancellationToCanonicalCoreAndFencedCompletion(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 7}, WorkerID: "worker-a", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Minute)}
	repository := &runnerRepository{input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"), StopOnFailure: true,
		Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 1, ManifestJSON: []byte("manifest")},
	}, cancelled: true}
	core := &cancellationCore{started: make(chan struct{})}
	runner, err := NewRunner(repository, staticProvider{artifact: &runnerArtifact{manifest: bundle.Manifest{
		SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact,
		Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		Cases:  []bundle.Case{{ID: "case-1", Input: "1.in", Output: "1.out", Weight: 1}},
	}}}, core, Config{LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond, ControlPollInterval: 5 * time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ExecuteClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if !core.cancelled || repository.completions != 1 || repository.infrastructureFailures != 0 {
		t.Fatalf("core cancelled=%v completions=%d infrastructure failures=%d", core.cancelled, repository.completions, repository.infrastructureFailures)
	}
}

func TestRunnerClaimsQueuedWorkAndStopsWithParentContext(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 9}, WorkerID: "worker-loop", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Minute)}
	completed := make(chan struct{}, 1)
	repository := &runnerRepository{claim: &claim, completionSignal: completed, input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"), StopOnFailure: true,
		Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 1, ManifestJSON: []byte("manifest")},
	}}
	artifact := &runnerArtifact{manifest: bundle.Manifest{
		SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact,
		Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
		Cases:  []bundle.Case{{ID: "case-1", Input: "1.in", Output: "1.out", Weight: 1}},
	}}
	runner, err := NewRunner(repository, staticProvider{artifact: artifact}, acceptedCore{}, Config{
		LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond,
		ControlPollInterval: 5 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, "worker-loop", time.Millisecond) }()
	select {
	case <-completed:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("runner did not complete claimed work")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunnerClaimLoopRetriesTransientDatabaseFailuresButReturnsPermanentErrors(t *testing.T) {
	transient := fmt.Errorf("claim: %w", &mysqlDriver.MySQLError{Number: 1205, Message: "lock wait timeout"})
	repository := &runnerRepository{claimErrors: []error{transient}, claimRetried: make(chan struct{})}
	runner, err := NewRunner(repository, staticProvider{}, acceptedCore{}, Config{
		LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond,
		ControlPollInterval: 10 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, "transient-claim-worker", time.Millisecond) }()
	select {
	case <-repository.claimRetried:
		cancel()
	case err := <-done:
		t.Fatalf("transient claim error stopped runner: %v", err)
	case <-time.After(time.Second):
		t.Fatal("runner did not retry transient claim error")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner shutdown error=%v", err)
	}

	permanent := errors.New("claim invariant")
	repository = &runnerRepository{claimErrors: []error{permanent}}
	runner, err = NewRunner(repository, staticProvider{}, acceptedCore{}, Config{
		LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond,
		ControlPollInterval: 10 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), "permanent-claim-worker", time.Millisecond); !errors.Is(err, permanent) {
		t.Fatalf("permanent claim error=%v", err)
	}
}

func TestRunnerClaimLoopRetriesTransientCompletionDatabaseFailure(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 10}, WorkerID: "completion-loop", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Minute)}
	repository := &runnerRepository{
		claim: &claim, claimRetried: make(chan struct{}),
		completeErr: fmt.Errorf("complete: %w", &mysqlDriver.MySQLError{Number: 1213, Message: "deadlock"}),
		input: external.WorkerExecutionInput{
			Language: "go126", SourceCode: []byte("package main"),
			Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: strings.Repeat("a", 64), SizeBytes: 1},
		},
	}
	runner, err := NewRunner(repository, staticProvider{artifact: &runnerArtifact{}}, acceptedCore{}, Config{
		LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond,
		ControlPollInterval: 10 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, "completion-loop", time.Millisecond) }()
	select {
	case <-repository.claimRetried:
		cancel()
	case err := <-done:
		t.Fatalf("transient completion error stopped runner: %v", err)
	case <-time.After(time.Second):
		t.Fatal("runner did not continue after transient completion error")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner shutdown error=%v", err)
	}
}

func TestRunnerHeartbeatsWhileBundleProviderIsBlocked(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 11}, WorkerID: "slow-bundle-worker", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Second)}
	heartbeat := make(chan struct{}, 1)
	repository := &runnerRepository{heartbeatSignal: heartbeat, input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"), StopOnFailure: true,
		Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 1, ManifestJSON: []byte("manifest")},
	}}
	provider := &blockingProvider{
		started: make(chan struct{}), release: make(chan struct{}),
		artifact: &runnerArtifact{manifest: bundle.Manifest{
			SchemaVersion: 1, JudgeMode: bundle.JudgeModeACM, Checker: bundle.CheckerExact,
			Limits: bundle.Limits{TimeLimitMillis: 1000, MemoryLimitMiB: 64},
			Cases:  []bundle.Case{{ID: "case-1", Input: "1.in", Output: "1.out", Weight: 1}},
		}},
	}
	runner, err := NewRunner(repository, provider, acceptedCore{}, Config{
		LeaseDuration: 100 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond,
		ControlPollInterval: 20 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runner.ExecuteClaim(context.Background(), claim) }()
	<-provider.started
	select {
	case <-heartbeat:
	case <-time.After(80 * time.Millisecond):
		close(provider.release)
		<-done
		t.Fatal("claim was not heartbeated while bundle I/O was blocked")
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerHeartbeatsWhileSlowControlReadTimesOutAndCancelsBundleIO(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 12}, WorkerID: "control-worker", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Second)}
	heartbeat := make(chan struct{}, 1)
	repository := &runnerRepository{controlBlock: true, heartbeatSignal: heartbeat, input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"), Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: strings.Repeat("a", 64), SizeBytes: 1},
	}}
	provider := &blockingProvider{started: make(chan struct{}), release: make(chan struct{}), artifact: &runnerArtifact{}}
	runner, err := NewRunner(repository, provider, acceptedCore{}, Config{LeaseDuration: time.Second, HeartbeatInterval: 5 * time.Millisecond, ControlPollInterval: 20 * time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ExecuteClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	select {
	case <-heartbeat:
	default:
		t.Fatal("slow control read suppressed lease heartbeats")
	}
	if repository.infrastructureFailures != 1 || repository.completions != 0 {
		t.Fatalf("infrastructure=%d completions=%d", repository.infrastructureFailures, repository.completions)
	}
}

func TestRunnerTreatsInvalidBundleAsInfrastructureFailure(t *testing.T) {
	claim := external.WorkerJobClaim{Job: external.ExternalJobRecord{InternalID: 13}, WorkerID: "invalid-bundle-worker", AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Second)}
	repository := &runnerRepository{input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"), Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: strings.Repeat("a", 64), SizeBytes: 1},
	}}
	runner, err := NewRunner(repository, staticProvider{err: bundle.Invalid(errors.New("bad manifest"))}, acceptedCore{}, Config{
		LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond, ControlPollInterval: 10 * time.Millisecond, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ExecuteClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if repository.infrastructureFailures != 1 || repository.completions != 0 {
		t.Fatalf("infrastructure=%d completions=%d", repository.infrastructureFailures, repository.completions)
	}
}

func TestRunnerChargesTenantSpecialJudgeFailuresToTheAttemptReservation(t *testing.T) {
	claim := external.WorkerJobClaim{
		Job: external.ExternalJobRecord{InternalID: 14}, WorkerID: "checker-worker",
		AttemptNo: 1, LeaseToken: make([]byte, 32), LeaseUntil: time.Now().Add(time.Second),
	}
	repository := &runnerRepository{input: external.WorkerExecutionInput{
		Language: "go126", SourceCode: []byte("package main"),
		Bundle: external.WorkerBundleInput{ObjectKey: "bundle.zip", SHA256: strings.Repeat("a", 64), SizeBytes: 1},
	}}
	runner, err := NewRunner(repository, staticProvider{artifact: &runnerArtifact{}},
		errorCore{err: fmt.Errorf("%w: checker compile failed", service.ErrTenantCheckerFailure)},
		Config{LeaseDuration: time.Second, HeartbeatInterval: 20 * time.Millisecond, ControlPollInterval: 10 * time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ExecuteClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if repository.infrastructureFailures != 1 ||
		repository.lastInfrastructureFailure.Code != "TENANT_CHECKER_FAILED" ||
		!repository.lastInfrastructureFailure.ChargeFullReservation ||
		!repository.lastInfrastructureFailure.Permanent {
		t.Fatalf("infrastructure failures=%d failure=%+v", repository.infrastructureFailures, repository.lastInfrastructureFailure)
	}
}

type runnerRepository struct {
	input                     external.WorkerExecutionInput
	cancelled                 bool
	completions               int
	infrastructureFailures    int
	lastInfrastructureFailure external.InfrastructureFailure
	claim                     *external.WorkerJobClaim
	completionSignal          chan struct{}
	heartbeatSignal           chan struct{}
	controlErr                error
	controlBlock              bool
	claimErrors               []error
	claimCalls                int
	claimRetried              chan struct{}
	completeErr               error
}

func (repository *runnerRepository) ClaimNext(context.Context, string, time.Duration) (external.WorkerJobClaim, error) {
	repository.claimCalls++
	if repository.claimCalls >= 2 && repository.claimRetried != nil {
		select {
		case <-repository.claimRetried:
		default:
			close(repository.claimRetried)
		}
	}
	if len(repository.claimErrors) > 0 {
		err := repository.claimErrors[0]
		repository.claimErrors = repository.claimErrors[1:]
		return external.WorkerJobClaim{}, err
	}
	if repository.claim == nil {
		return external.WorkerJobClaim{}, external.ErrJobNotClaimable
	}
	claim := *repository.claim
	repository.claim = nil
	return claim, nil
}

func (repository *runnerRepository) LoadClaimInput(context.Context, external.WorkerJobClaim) (external.WorkerExecutionInput, error) {
	return repository.input, nil
}
func (repository *runnerRepository) Heartbeat(context.Context, external.WorkerJobClaim, time.Duration) error {
	if repository.heartbeatSignal != nil {
		select {
		case repository.heartbeatSignal <- struct{}{}:
		default:
		}
	}
	return nil
}
func (repository *runnerRepository) ClaimCancelled(ctx context.Context, _ external.WorkerJobClaim) (bool, error) {
	if repository.controlBlock {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return repository.cancelled, repository.controlErr
}
func (repository *runnerRepository) Complete(_ context.Context, _ external.WorkerJobClaim, _ external.DurableJobResult) error {
	repository.completions++
	if repository.completeErr != nil {
		return repository.completeErr
	}
	if repository.completionSignal != nil {
		repository.completionSignal <- struct{}{}
	}
	return nil
}
func (repository *runnerRepository) FailInfrastructure(_ context.Context, _ external.WorkerJobClaim, failure external.InfrastructureFailure) (external.FailureDisposition, error) {
	repository.infrastructureFailures++
	repository.lastInfrastructureFailure = failure
	return external.FailureRequeued, nil
}

type staticProvider struct {
	artifact bundle.ArtifactReader
	err      error
}

func (provider staticProvider) OpenMetadata(context.Context, bundle.Metadata, []byte) (bundle.ArtifactReader, error) {
	return provider.artifact, provider.err
}

type blockingProvider struct {
	started  chan struct{}
	release  chan struct{}
	artifact bundle.ArtifactReader
}

func (provider *blockingProvider) OpenMetadata(ctx context.Context, _ bundle.Metadata, _ []byte) (bundle.ArtifactReader, error) {
	close(provider.started)
	select {
	case <-provider.release:
		return provider.artifact, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type cancellationCore struct {
	started   chan struct{}
	cancelled bool
}

func (core *cancellationCore) ExecuteCanonical(ctx context.Context, _ service.CanonicalExecutionRequest, _ service.CaseArtifact) (service.CanonicalResult, error) {
	close(core.started)
	<-ctx.Done()
	core.cancelled = errors.Is(ctx.Err(), context.Canceled)
	return service.CanonicalResult{}, ctx.Err()
}

type runnerArtifact struct{ manifest bundle.Manifest }

func (artifact *runnerArtifact) Manifest() bundle.Manifest                    { return artifact.manifest }
func (artifact *runnerArtifact) ReadCase(bundle.Case) (string, string, error) { return "", "", nil }
func (artifact *runnerArtifact) Close() error                                 { return nil }

type acceptedCore struct{}

func (acceptedCore) ExecuteCanonical(context.Context, service.CanonicalExecutionRequest, service.CaseArtifact) (service.CanonicalResult, error) {
	return service.CanonicalResult{Status: callback.StatusAccepted}, nil
}

type errorCore struct{ err error }

func (core errorCore) ExecuteCanonical(context.Context, service.CanonicalExecutionRequest, service.CaseArtifact) (service.CanonicalResult, error) {
	return service.CanonicalResult{}, core.err
}
