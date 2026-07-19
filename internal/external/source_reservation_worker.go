package external

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultSourceReservationMinimumAge    = time.Hour
	defaultSourceReservationSweepInterval = time.Hour
	defaultSourceReservationRevisitDelay  = time.Second
	sourceReservationSweepBatch           = 100
	sourceReservationSweepBudget          = 100
)

// SourceReservationSweeper is the durable repository boundary used to remove
// expired, unreferenced encrypted source uploads.
type SourceReservationSweeper interface {
	SweepSourceReservations(context.Context, time.Duration, int) (int, error)
}

// SourceReservationWorkerConfig controls orphan source-object reconciliation.
// Zero durations select conservative production defaults.
type SourceReservationWorkerConfig struct {
	Repository    SourceReservationSweeper
	MinimumAge    time.Duration
	SweepInterval time.Duration
	RevisitDelay  time.Duration
}

// SourceReservationWorker continuously reconciles durable source reservations.
type SourceReservationWorker struct {
	repository    SourceReservationSweeper
	minimumAge    time.Duration
	sweepInterval time.Duration
	revisitDelay  time.Duration
	wait          func(context.Context, time.Duration) error
}

// NewSourceReservationWorker builds the production source reservation sweeper.
func NewSourceReservationWorker(config SourceReservationWorkerConfig) (*SourceReservationWorker, error) {
	if config.MinimumAge == 0 {
		config.MinimumAge = defaultSourceReservationMinimumAge
	}
	if config.SweepInterval == 0 {
		config.SweepInterval = defaultSourceReservationSweepInterval
	}
	if config.RevisitDelay == 0 {
		config.RevisitDelay = defaultSourceReservationRevisitDelay
	}
	if config.Repository == nil || config.MinimumAge <= 0 || config.MinimumAge > 7*24*time.Hour ||
		config.SweepInterval <= 0 || config.RevisitDelay <= 0 || config.RevisitDelay > config.SweepInterval {
		return nil, fmt.Errorf("source reservation repository and positive sweep durations are required")
	}
	return &SourceReservationWorker{
		repository: config.Repository, minimumAge: config.MinimumAge,
		sweepInterval: config.SweepInterval, revisitDelay: config.RevisitDelay,
		wait: waitForSourceReservationSweep,
	}, nil
}

// Run sweeps immediately, then waits for either the normal interval or a
// prompt revisit when a bounded pass leaves a known backlog.
func (worker *SourceReservationWorker) Run(ctx context.Context) error {
	if worker == nil || worker.repository == nil || worker.wait == nil {
		return fmt.Errorf("source reservation worker is not configured")
	}
	for {
		drained, err := worker.sweep(ctx)
		if err != nil {
			return err
		}
		delay := worker.revisitDelay
		if drained {
			delay = worker.sweepInterval
		}
		if err := worker.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (worker *SourceReservationWorker) sweep(ctx context.Context) (bool, error) {
	for batch := 0; batch < sourceReservationSweepBudget; batch++ {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		reaped, err := worker.repository.SweepSourceReservations(ctx, worker.minimumAge, sourceReservationSweepBatch)
		if err != nil {
			return false, err
		}
		if reaped < 0 || reaped > sourceReservationSweepBatch {
			return false, fmt.Errorf("source reservation sweep returned invalid batch size %d", reaped)
		}
		if reaped < sourceReservationSweepBatch {
			return true, nil
		}
	}
	return false, nil
}

func waitForSourceReservationSweep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
