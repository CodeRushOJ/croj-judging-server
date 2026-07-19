package service

import (
	"context"
	"fmt"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type ArtifactProvider interface {
	Open(context.Context, *model.TestBundle) (bundle.ArtifactReader, error)
}

type HiddenTestExecutor struct {
	provider ArtifactProvider
	pipeline ArtifactExecutionPipeline
}

type ArtifactExecutionPipeline interface {
	ExecuteArtifact(context.Context, *model.Task, ExecutionConfig, CaseArtifact) (callback.Result, error)
}

func NewHiddenTestExecutor(provider ArtifactProvider, pipeline ArtifactExecutionPipeline) *HiddenTestExecutor {
	return &HiddenTestExecutor{provider: provider, pipeline: pipeline}
}

func (executor *HiddenTestExecutor) Execute(
	ctx context.Context,
	submission *model.Task,
	executionConfig ExecutionConfig,
	metadata *model.TestBundle,
) (result callback.Result, resultErr error) {
	if executor == nil || executor.provider == nil || executor.pipeline == nil {
		return callback.Result{}, fmt.Errorf("hidden test executor is not configured")
	}
	artifact, err := executor.provider.Open(ctx, metadata)
	if err != nil {
		if bundle.IsInvalid(err) {
			return systemErrorResult("immutable test bundle is invalid"), nil
		}
		return callback.Result{}, fmt.Errorf("load immutable test bundle: %w", err)
	}
	defer func() {
		if closeErr := artifact.Close(); closeErr != nil && resultErr == nil {
			result = systemErrorResult("immutable test bundle could not be closed")
		}
	}()
	return executor.pipeline.ExecuteArtifact(ctx, submission, executionConfig, artifact)
}
