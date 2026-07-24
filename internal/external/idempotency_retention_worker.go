package external

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type IdempotencyRetentionRepository interface {
	ExpireIdempotencyBatch(context.Context, int) (int64, error)
}

type IdempotencyRetentionWorker struct {
	repository IdempotencyRetentionRepository
	batch      int
	idleDelay  time.Duration
}

func NewIdempotencyRetentionWorker(repository IdempotencyRetentionRepository, batch int, idleDelay time.Duration) (*IdempotencyRetentionWorker, error) {
	if repository == nil || batch < 1 || batch > 1000 || idleDelay <= 0 || idleDelay > time.Hour {
		return nil, fmt.Errorf("idempotency retention repository, batch, and idle delay are required")
	}
	return &IdempotencyRetentionWorker{repository: repository, batch: batch, idleDelay: idleDelay}, nil
}

func (worker *IdempotencyRetentionWorker) Run(ctx context.Context) error {
	for {
		count, err := worker.repository.ExpireIdempotencyBatch(ctx, worker.batch)
		if err != nil && !errors.Is(err, ErrIdempotencyRetentionNotAvailable) && !IsTransientDatabaseError(err) {
			return err
		}
		if err == nil && count == int64(worker.batch) {
			continue
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
