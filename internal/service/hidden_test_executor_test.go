package service

import (
	"context"
	"errors"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/bundle"
	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
)

type fakeArtifactProvider struct {
	artifact bundle.ArtifactReader
	err      error
}

func (provider fakeArtifactProvider) Open(context.Context, *model.TestBundle) (bundle.ArtifactReader, error) {
	return provider.artifact, provider.err
}

func TestHiddenTestExecutorReturnsSystemErrorForInvalidBundle(t *testing.T) {
	executor := NewHiddenTestExecutor(fakeArtifactProvider{err: bundle.Invalid(errors.New("bad ZIP"))}, &BundlePipeline{})
	result, err := executor.Execute(context.Background(), validBundleSubmission(), validBundleProblem(), &model.TestBundle{})
	if err != nil || result.Status != callback.StatusSystemError {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestHiddenTestExecutorKeepsTransientStorageFailureRetryable(t *testing.T) {
	wantErr := errors.New("object store unavailable")
	executor := NewHiddenTestExecutor(fakeArtifactProvider{err: wantErr}, &BundlePipeline{})
	_, err := executor.Execute(context.Background(), validBundleSubmission(), validBundleProblem(), &model.TestBundle{})
	if !errors.Is(err, wantErr) || callback.IsPermanent(err) {
		t.Fatalf("error=%v permanent=%v", err, callback.IsPermanent(err))
	}
}

type trackingArtifact struct {
	*memoryArtifact
	closed bool
}

func (artifact *trackingArtifact) Close() error {
	artifact.closed = true
	return nil
}

func TestHiddenTestExecutorClosesArtifactWhenPipelineFails(t *testing.T) {
	artifact := &trackingArtifact{memoryArtifact: exactArtifact(1)}
	pipeline := NewBundlePipeline(&sequenceSelector{}, &sequenceExecutor{}, 1)
	executor := NewHiddenTestExecutor(fakeArtifactProvider{artifact: artifact}, pipeline)
	if _, err := executor.Execute(context.Background(), validBundleSubmission(), validBundleProblem(), &model.TestBundle{}); err == nil {
		t.Fatal("expected pipeline error")
	}
	if !artifact.closed {
		t.Fatal("artifact was not closed")
	}
}
