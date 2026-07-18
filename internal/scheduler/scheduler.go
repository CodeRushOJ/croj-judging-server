package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type Discovery interface {
	Endpoints(context.Context) ([]string, error)
}

// Scheduler keeps the last successful EndpointSlice snapshot and selects
// sandboxes using deterministic round robin.
type Scheduler struct {
	discovery Discovery
	mu        sync.Mutex
	endpoints []string
	next      uint64
}

func New(discovery Discovery) *Scheduler {
	return &Scheduler{discovery: discovery}
}

func (s *Scheduler) Refresh(ctx context.Context) error {
	endpoints, err := s.discovery.Endpoints(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.endpoints = append(s.endpoints[:0], endpoints...)
	if len(s.endpoints) == 0 {
		s.next = 0
	} else {
		s.next %= uint64(len(s.endpoints))
	}
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := s.Refresh(ctx); err != nil {
		log.Printf("initial sandbox discovery failed: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Refresh(ctx); err != nil {
				log.Printf("sandbox discovery refresh failed; keeping last known endpoints: %v", err)
			}
		}
	}
}

func (s *Scheduler) SelectSandbox() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.endpoints) == 0 {
		return "", fmt.Errorf("no ready sandbox endpoints")
	}
	selected := s.endpoints[s.next%uint64(len(s.endpoints))]
	s.next++
	return selected, nil
}
