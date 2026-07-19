package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
)

func TestDurableResultPreservesCanonicalVerdictCasesAndUnits(t *testing.T) {
	result, err := durableResult(service.CanonicalResult{
		Status: callback.StatusWrongAnswer, TimeUsedMillis: 17, MemoryUsedKB: 2048,
		Cases: []service.CanonicalCaseResult{
			{CaseID: "case-1", Status: callback.StatusAccepted, TimeUsedMillis: 11, MemoryUsedKB: 1024},
			{CaseID: "case-2", Status: callback.StatusWrongAnswer, TimeUsedMillis: 17, MemoryUsedKB: 2048},
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

	compile, err := durableResult(service.CanonicalResult{Status: callback.StatusCompileError, CompileError: "redacted"})
	if err != nil || compile.CompileStatus != "FAILED" || compile.Verdict != "COMPILE_ERROR" {
		t.Fatalf("compile result=%+v error=%v", compile, err)
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

type runnerRepository struct {
	input                  external.WorkerExecutionInput
	cancelled              bool
	completions            int
	infrastructureFailures int
	claim                  *external.WorkerJobClaim
	completionSignal       chan struct{}
	heartbeatSignal        chan struct{}
}

func (repository *runnerRepository) ClaimNext(context.Context, string, time.Duration) (external.WorkerJobClaim, error) {
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
func (repository *runnerRepository) ClaimCancelled(context.Context, external.WorkerJobClaim) (bool, error) {
	return repository.cancelled, nil
}
func (repository *runnerRepository) Complete(_ context.Context, _ external.WorkerJobClaim, _ external.DurableJobResult) error {
	repository.completions++
	if repository.completionSignal != nil {
		repository.completionSignal <- struct{}{}
	}
	return nil
}
func (repository *runnerRepository) FailInfrastructure(context.Context, external.WorkerJobClaim, external.InfrastructureFailure) (external.FailureDisposition, error) {
	repository.infrastructureFailures++
	return external.FailureRequeued, nil
}

type staticProvider struct{ artifact bundle.ArtifactReader }

func (provider staticProvider) OpenMetadata(context.Context, bundle.Metadata, []byte) (bundle.ArtifactReader, error) {
	return provider.artifact, nil
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
