package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type fakeDiscovery struct {
	mu        sync.RWMutex
	endpoints []string
	err       error
}

func TestSchedulerSelectSandboxExcludingIsConcurrentAndChurnSafe(t *testing.T) {
	provider := &fakeDiscovery{endpoints: []string{"sandbox-a", "sandbox-b", "sandbox-c"}}
	scheduler := New(provider)
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	const selections = 100
	selected := make(chan string, selections)
	var wait sync.WaitGroup
	for range selections {
		wait.Add(1)
		go func() {
			defer wait.Done()
			endpoint, err := scheduler.SelectSandboxExcluding(map[string]struct{}{"sandbox-a": {}})
			if err != nil {
				t.Errorf("SelectSandboxExcluding: %v", err)
				return
			}
			selected <- endpoint
		}()
	}
	wait.Wait()
	close(selected)
	counts := map[string]int{}
	for endpoint := range selected {
		counts[endpoint]++
	}
	if counts["sandbox-a"] != 0 || counts["sandbox-b"] != 50 || counts["sandbox-c"] != 50 {
		t.Fatalf("concurrent selection counts = %v", counts)
	}

	churnErrors := make(chan error, selections)
	var churnWait sync.WaitGroup
	for index := range selections {
		churnWait.Add(1)
		go func(index int) {
			defer churnWait.Done()
			if index%2 == 0 {
				provider.set([]string{"sandbox-a", "sandbox-b", "sandbox-c"}, nil)
			} else {
				provider.set([]string{"sandbox-a", "sandbox-c", "sandbox-d"}, nil)
			}
			if err := scheduler.Refresh(context.Background()); err != nil {
				churnErrors <- err
				return
			}
			endpoint, err := scheduler.SelectSandboxExcluding(map[string]struct{}{"sandbox-a": {}})
			if err != nil {
				churnErrors <- err
				return
			}
			if endpoint == "sandbox-a" {
				churnErrors <- errors.New("selected excluded sandbox-a during churn")
			}
		}(index)
	}
	churnWait.Wait()
	close(churnErrors)
	for err := range churnErrors {
		t.Error(err)
	}

	provider.set([]string{"sandbox-a", "sandbox-d"}, nil)
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	endpoint, err := scheduler.SelectSandboxExcluding(map[string]struct{}{"sandbox-a": {}, "sandbox-b": {}, "sandbox-c": {}})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "sandbox-d" {
		t.Fatalf("post-churn endpoint = %q, want sandbox-d", endpoint)
	}
}

func (f *fakeDiscovery) Endpoints(context.Context) ([]string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.endpoints...), f.err
}

func (f *fakeDiscovery) set(endpoints []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints = append([]string(nil), endpoints...)
	f.err = err
}

func TestSchedulerUsesDeterministicRoundRobin(t *testing.T) {
	provider := &fakeDiscovery{endpoints: []string{"sandbox-a:8080", "sandbox-b:8080"}}
	scheduler := New(provider)
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}

	var got []string
	for range 3 {
		endpoint, err := scheduler.SelectSandbox()
		if err != nil {
			t.Fatalf("SelectSandbox returned error: %v", err)
		}
		got = append(got, endpoint)
	}
	want := []string{"sandbox-a:8080", "sandbox-b:8080", "sandbox-a:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
}

func TestRefreshKeepsLastKnownGoodEndpointsOnDiscoveryFailure(t *testing.T) {
	provider := &fakeDiscovery{endpoints: []string{"sandbox-a:8080"}}
	scheduler := New(provider)
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh returned error: %v", err)
	}
	provider.set(nil, errors.New("kubernetes API unavailable"))

	if err := scheduler.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	endpoint, err := scheduler.SelectSandbox()
	if err != nil {
		t.Fatalf("SelectSandbox returned error after refresh failure: %v", err)
	}
	if endpoint != "sandbox-a:8080" {
		t.Fatalf("endpoint = %q, want last known good endpoint", endpoint)
	}
}
