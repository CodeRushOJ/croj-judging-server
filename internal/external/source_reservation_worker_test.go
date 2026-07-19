package external

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type sourceReservationSweeperStub struct {
	mu         sync.Mutex
	results    []int
	err        error
	calls      int
	ages       []time.Duration
	limits     []int
	started    chan struct{}
	startedOne sync.Once
}

func (repository *sourceReservationSweeperStub) SweepSourceReservations(
	ctx context.Context,
	minimumAge time.Duration,
	limit int,
) (int, error) {
	repository.startedOne.Do(func() {
		if repository.started != nil {
			close(repository.started)
		}
	})
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.calls++
	repository.ages = append(repository.ages, minimumAge)
	repository.limits = append(repository.limits, limit)
	if repository.err != nil {
		return 0, repository.err
	}
	if len(repository.results) == 0 {
		return 0, nil
	}
	result := repository.results[0]
	repository.results = repository.results[1:]
	return result, nil
}

func (repository *sourceReservationSweeperStub) snapshot() (int, []time.Duration, []int) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.calls, append([]time.Duration(nil), repository.ages...), append([]int(nil), repository.limits...)
}

func newSourceReservationWorkerForTest(t *testing.T, repository SourceReservationSweeper) *SourceReservationWorker {
	t.Helper()
	worker, err := NewSourceReservationWorker(SourceReservationWorkerConfig{
		Repository:    repository,
		MinimumAge:    30 * time.Minute,
		SweepInterval: 2 * time.Hour,
		RevisitDelay:  2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestSourceReservationWorkerRejectsInvalidConfiguration(t *testing.T) {
	repository := &sourceReservationSweeperStub{}
	for name, config := range map[string]SourceReservationWorkerConfig{
		"repository": {},
		"minimum age": {
			Repository: repository, MinimumAge: -time.Second,
		},
		"sweep interval": {
			Repository: repository, SweepInterval: -time.Second,
		},
		"revisit delay": {
			Repository: repository, RevisitDelay: -time.Second,
		},
		"revisit slower than interval": {
			Repository: repository, SweepInterval: time.Minute, RevisitDelay: 2 * time.Minute,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSourceReservationWorker(config); err == nil {
				t.Fatal("invalid source reservation worker configuration was accepted")
			}
		})
	}
}

func TestSourceReservationWorkerSweepsImmediatelyAndDrainsFullBatches(t *testing.T) {
	repository := &sourceReservationSweeperStub{results: []int{100, 100, 17}}
	worker := newSourceReservationWorkerForTest(t, repository)
	var waited []time.Duration
	stop := errors.New("stop after drained pass")
	worker.wait = func(_ context.Context, delay time.Duration) error {
		waited = append(waited, delay)
		return stop
	}

	err := worker.Run(context.Background())
	if !errors.Is(err, stop) {
		t.Fatalf("Run error=%v want=%v", err, stop)
	}
	calls, ages, limits := repository.snapshot()
	if calls != 3 {
		t.Fatalf("sweeps=%d want=3", calls)
	}
	for index := range ages {
		if ages[index] != 30*time.Minute || limits[index] != 100 {
			t.Fatalf("sweep[%d] age=%v limit=%d", index, ages[index], limits[index])
		}
	}
	if len(waited) != 1 || waited[0] != 2*time.Hour {
		t.Fatalf("waits=%v want=[2h]", waited)
	}
}

func TestSourceReservationWorkerBoundsPassAndPromptlyRevisitsBacklog(t *testing.T) {
	results := make([]int, 100)
	for index := range results {
		results[index] = 100
	}
	repository := &sourceReservationSweeperStub{results: results}
	worker := newSourceReservationWorkerForTest(t, repository)
	stop := errors.New("stop after bounded pass")
	var waited []time.Duration
	worker.wait = func(_ context.Context, delay time.Duration) error {
		waited = append(waited, delay)
		return stop
	}

	err := worker.Run(context.Background())
	if !errors.Is(err, stop) {
		t.Fatalf("Run error=%v want=%v", err, stop)
	}
	calls, _, _ := repository.snapshot()
	if calls != 100 {
		t.Fatalf("sweeps=%d want bounded pass of 100", calls)
	}
	if len(waited) != 1 || waited[0] != 2*time.Second {
		t.Fatalf("waits=%v want prompt 2s revisit", waited)
	}
}

func TestSourceReservationWorkerReturnsSweepFailure(t *testing.T) {
	failed := errors.New("mysql unavailable")
	repository := &sourceReservationSweeperStub{err: failed}
	worker := newSourceReservationWorkerForTest(t, repository)

	if err := worker.Run(context.Background()); !errors.Is(err, failed) {
		t.Fatalf("Run error=%v want=%v", err, failed)
	}
}

func TestSourceReservationWorkerCancellationInterruptsIntervalWait(t *testing.T) {
	repository := &sourceReservationSweeperStub{started: make(chan struct{})}
	worker := newSourceReservationWorkerForTest(t, repository)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-repository.started
	cancel(context.Canceled)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop promptly after cancellation")
	}
}
