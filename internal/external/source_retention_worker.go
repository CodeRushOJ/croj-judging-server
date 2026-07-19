package external

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrSourceRetentionDeleteFailed = errors.New("source retention object deletion failed")

type SourceRetentionRepository interface {
	ClaimSourceRetention(context.Context, time.Duration) (SourceRetentionClaim, error)
	RecordSourceRetentionFailure(context.Context, SourceRetentionClaim) error
	FinalizeSourceRetention(context.Context, SourceRetentionClaim) error
}

type SourceRetentionWorkerConfig struct {
	Repository    SourceRetentionRepository
	Objects       SourceObjectStore
	Retention     time.Duration
	IdleDelay     time.Duration
	DeleteTimeout time.Duration
}

type SourceRetentionWorker struct {
	repository    SourceRetentionRepository
	objects       SourceObjectStore
	retention     time.Duration
	idleDelay     time.Duration
	deleteTimeout time.Duration
}

func NewSourceRetentionWorker(config SourceRetentionWorkerConfig) (*SourceRetentionWorker, error) {
	if config.Retention == 0 {
		config.Retention = 30 * 24 * time.Hour
	}
	if config.IdleDelay == 0 {
		config.IdleDelay = time.Minute
	}
	if config.DeleteTimeout == 0 {
		config.DeleteTimeout = 30 * time.Second
	}
	if config.Repository == nil || config.Objects == nil || config.Retention < time.Hour || config.Retention > 365*24*time.Hour ||
		config.IdleDelay <= 0 || config.IdleDelay > time.Hour || config.DeleteTimeout <= 0 || config.DeleteTimeout > time.Minute {
		return nil, fmt.Errorf("source retention repository, object store, and bounded durations are required")
	}
	return &SourceRetentionWorker{repository: config.Repository, objects: config.Objects, retention: config.Retention, idleDelay: config.IdleDelay, deleteTimeout: config.DeleteTimeout}, nil
}

func (worker *SourceRetentionWorker) ProcessNext(ctx context.Context) error {
	if worker == nil || worker.repository == nil || worker.objects == nil {
		return fmt.Errorf("source retention worker is not configured")
	}
	claim, err := worker.repository.ClaimSourceRetention(ctx, worker.retention)
	if err != nil {
		return err
	}
	deleteContext, cancel := context.WithTimeout(ctx, worker.deleteTimeout)
	err = worker.objects.Delete(deleteContext, claim.ObjectKey)
	cancel()
	if err != nil {
		if recordErr := worker.repository.RecordSourceRetentionFailure(ctx, claim); recordErr != nil {
			return errors.Join(ErrSourceRetentionDeleteFailed, recordErr)
		}
		return ErrSourceRetentionDeleteFailed
	}
	return worker.repository.FinalizeSourceRetention(ctx, claim)
}

func (worker *SourceRetentionWorker) Run(ctx context.Context) error {
	for {
		err := worker.ProcessNext(ctx)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrSourceRetentionNotAvailable) && !errors.Is(err, ErrSourceRetentionDeleteFailed) {
			return err
		}
		timer := time.NewTimer(worker.idleDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}
