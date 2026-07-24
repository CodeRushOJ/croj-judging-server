package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/internal/external"
	"github.com/CodeRushOJ/croj-judging-server/internal/service"
)

var errCancellationRequested = errors.New("durable job cancellation requested")

type Repository interface {
	LoadClaimInput(context.Context, external.WorkerJobClaim) (external.WorkerExecutionInput, error)
	Heartbeat(context.Context, external.WorkerJobClaim, time.Duration) error
	ClaimCancelled(context.Context, external.WorkerJobClaim) (bool, error)
	Complete(context.Context, external.WorkerJobClaim, external.DurableJobResult) error
	FailInfrastructure(context.Context, external.WorkerJobClaim, external.InfrastructureFailure) (external.FailureDisposition, error)
}

type ClaimRepository interface {
	Repository
	ClaimNext(context.Context, string, time.Duration) (external.WorkerJobClaim, error)
}

type ArtifactProvider interface {
	OpenMetadata(context.Context, bundle.Metadata, []byte) (bundle.ArtifactReader, error)
}

type CanonicalCore interface {
	ExecuteCanonical(context.Context, service.CanonicalExecutionRequest, service.CaseArtifact) (service.CanonicalResult, error)
}

type Config struct {
	LeaseDuration       time.Duration
	HeartbeatInterval   time.Duration
	ControlPollInterval time.Duration
	RetryDelay          time.Duration
}

type Runner struct {
	repository Repository
	provider   ArtifactProvider
	core       CanonicalCore
	config     Config
}

func NewRunner(repository Repository, provider ArtifactProvider, core CanonicalCore, config Config) (*Runner, error) {
	if repository == nil || provider == nil || core == nil || config.LeaseDuration <= 0 || config.LeaseDuration > 15*time.Minute ||
		config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= config.LeaseDuration ||
		config.ControlPollInterval <= 0 || config.ControlPollInterval >= config.LeaseDuration ||
		config.RetryDelay < 0 || config.RetryDelay > time.Hour {
		return nil, fmt.Errorf("durable worker configuration is invalid")
	}
	return &Runner{repository: repository, provider: provider, core: core, config: config}, nil
}

func (runner *Runner) ExecuteClaim(ctx context.Context, claim external.WorkerJobClaim) error {
	executionContext, cancel := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	controlResult := make(chan error, 1)
	go runner.monitorClaim(executionContext, cancel, claim, controlDone, controlResult)
	result, failureCode, executionErr := runner.executeControlled(executionContext, claim)
	close(controlDone)
	controlErr := <-controlResult
	cancel()

	if errors.Is(controlErr, errCancellationRequested) {
		return runner.completeCancellation(ctx, claim)
	}
	if controlErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(controlErr, external.ErrStaleJobClaim) {
			return nil
		}
		return runner.failInfrastructure(ctx, claim, "LEASE_CONTROL_FAILED")
	}
	if executionErr != nil {
		if errors.Is(executionErr, external.ErrStaleJobClaim) {
			return nil
		}
		if errors.Is(executionErr, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		return runner.failInfrastructure(ctx, claim, failureCode)
	}
	return runner.complete(ctx, claim, result)
}

func (runner *Runner) executeControlled(ctx context.Context, claim external.WorkerJobClaim) (service.CanonicalResult, string, error) {
	input, err := runner.repository.LoadClaimInput(ctx, claim)
	if err != nil {
		return service.CanonicalResult{}, "LOAD_EXECUTION_INPUT_FAILED", err
	}
	defer clear(input.SourceCode)
	artifact, err := runner.provider.OpenMetadata(ctx, bundle.Metadata{
		ObjectKey: input.Bundle.ObjectKey, SHA256: input.Bundle.SHA256, SizeBytes: input.Bundle.SizeBytes,
	}, input.Bundle.ManifestJSON)
	if err != nil {
		if bundle.IsInvalid(err) {
			return service.CanonicalResult{}, "LOAD_BUNDLE_INVALID", err
		}
		return service.CanonicalResult{}, "LOAD_BUNDLE_FAILED", err
	}
	defer artifact.Close()
	result, err := runner.core.ExecuteCanonical(ctx, service.CanonicalExecutionRequest{
		Language: input.Language, SourceCode: string(input.SourceCode), StopOnFailure: input.StopOnFailure,
	}, artifact)
	return result, "SANDBOX_EXECUTION_FAILED", err
}

func (runner *Runner) completeCancellation(ctx context.Context, claim external.WorkerJobClaim) error {
	// Complete rechecks cancel_requested_at under the lease fence and discards this
	// valid placeholder instead of persisting a contestant result.
	err := runner.repository.Complete(ctx, claim, external.DurableJobResult{Verdict: string(callback.StatusAccepted), CompileStatus: "SUCCEEDED"})
	if errors.Is(err, external.ErrStaleJobClaim) {
		return nil
	}
	return err
}

func (runner *Runner) Run(ctx context.Context, workerID string, idleBackoff time.Duration) error {
	repository, ok := runner.repository.(ClaimRepository)
	if !ok || workerID == "" || idleBackoff <= 0 || idleBackoff > time.Minute {
		return fmt.Errorf("durable worker claim loop is invalid")
	}
	for {
		claim, err := repository.ClaimNext(ctx, workerID, runner.config.LeaseDuration)
		if errors.Is(err, external.ErrJobNotClaimable) {
			if err := waitForRepositoryRetry(ctx, idleBackoff); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if external.IsTransientDatabaseError(err) {
				if err := waitForRepositoryRetry(ctx, idleBackoff); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("claim durable judge job: %w", err)
		}
		if err := runner.ExecuteClaim(ctx, claim); err != nil {
			if external.IsTransientDatabaseError(err) {
				if err := waitForRepositoryRetry(ctx, idleBackoff); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
}

func waitForRepositoryRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func (runner *Runner) monitorClaim(ctx context.Context, cancel context.CancelFunc, claim external.WorkerJobClaim, done <-chan struct{}, result chan<- error) {
	monitorContext, stopMonitor := context.WithCancel(ctx)
	events := make(chan error, 2)
	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); runner.heartbeatLoop(monitorContext, claim, events) }()
	go func() { defer loops.Done(); runner.controlLoop(monitorContext, claim, events) }()
	var monitorErr error
	select {
	case <-done:
	case <-ctx.Done():
		monitorErr = ctx.Err()
	case monitorErr = <-events:
		cancel()
	}
	stopMonitor()
	loops.Wait()
	result <- monitorErr
}

func (runner *Runner) heartbeatLoop(ctx context.Context, claim external.WorkerJobClaim, events chan<- error) {
	ticker := time.NewTicker(runner.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callContext, cancel := context.WithTimeout(ctx, runner.config.HeartbeatInterval)
			err := runner.repository.Heartbeat(callContext, claim, runner.config.LeaseDuration)
			cancel()
			if err != nil {
				events <- err
				return
			}
		}
	}
}

func (runner *Runner) controlLoop(ctx context.Context, claim external.WorkerJobClaim, events chan<- error) {
	ticker := time.NewTicker(runner.config.ControlPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callContext, cancel := context.WithTimeout(ctx, runner.config.ControlPollInterval)
			cancelled, err := runner.repository.ClaimCancelled(callContext, claim)
			cancel()
			if err != nil {
				events <- err
				return
			}
			if cancelled {
				events <- errCancellationRequested
				return
			}
		}
	}
}

func (runner *Runner) complete(ctx context.Context, claim external.WorkerJobClaim, result service.CanonicalResult) error {
	durable, err := durableResult(result)
	if err != nil {
		return runner.failInfrastructure(ctx, claim, "INVALID_EXECUTION_RESULT")
	}
	err = runner.repository.Complete(ctx, claim, durable)
	if errors.Is(err, external.ErrStaleJobClaim) {
		return nil
	}
	return err
}

func (runner *Runner) failInfrastructure(ctx context.Context, claim external.WorkerJobClaim, code string) error {
	_, err := runner.repository.FailInfrastructure(ctx, claim, external.InfrastructureFailure{Code: code, RetryDelay: runner.config.RetryDelay})
	if errors.Is(err, external.ErrStaleJobClaim) {
		return nil
	}
	return err
}

func durableResult(result service.CanonicalResult) (external.DurableJobResult, error) {
	if !durableVerdict(result.Status) || result.TimeUsedMillis < 0 || result.MemoryUsedKB < 0 || int64(result.MemoryUsedKB) > math.MaxInt64/1024 {
		return external.DurableJobResult{}, fmt.Errorf("canonical execution result is invalid")
	}
	compileStatus := "SUCCEEDED"
	if result.Status == callback.StatusCompileError {
		compileStatus = "FAILED"
	}
	durable := external.DurableJobResult{
		Verdict: string(result.Status), CompileStatus: compileStatus,
		TimeMillis: int64(result.TimeUsedMillis), MemoryBytes: int64(result.MemoryUsedKB) * 1024,
		Cases: make([]external.DurableCaseResult, 0, len(result.Cases)),
	}
	for _, item := range result.Cases {
		if item.CaseID == "" || !durableVerdict(item.Status) || item.TimeUsedMillis < 0 || item.MemoryUsedKB < 0 || int64(item.MemoryUsedKB) > math.MaxInt64/1024 {
			return external.DurableJobResult{}, fmt.Errorf("canonical case result is invalid")
		}
		durable.Cases = append(durable.Cases, external.DurableCaseResult{
			CaseID: item.CaseID, Verdict: string(item.Status),
			TimeMillis: int64(item.TimeUsedMillis), MemoryBytes: int64(item.MemoryUsedKB) * 1024,
		})
	}
	return durable, nil
}

func durableVerdict(status callback.Status) bool {
	switch status {
	case callback.StatusAccepted, callback.StatusWrongAnswer, callback.StatusCompileError,
		callback.StatusTimeLimitExceeded, callback.StatusMemoryLimitExceeded,
		callback.StatusRuntimeError:
		return true
	default:
		return false
	}
}
