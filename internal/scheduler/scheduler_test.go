package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeDiscovery struct {
	endpoints []string
	err       error
}

func (f *fakeDiscovery) Endpoints(context.Context) ([]string, error) {
	return append([]string(nil), f.endpoints...), f.err
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
	provider.endpoints = nil
	provider.err = errors.New("kubernetes API unavailable")

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
