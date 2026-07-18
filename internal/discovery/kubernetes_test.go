package discovery

import (
	"reflect"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
)

func TestEndpointAddressesIncludesOnlyReadyNonTerminatingBackends(t *testing.T) {
	ready := true
	notReady := false
	terminating := true
	notTerminating := false
	portName := "grpc"
	port := int32(8080)
	slices := []discoveryv1.EndpointSlice{
		{
			Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready, Terminating: &notTerminating}},
				{Addresses: []string{"10.0.0.4"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
				{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready, Terminating: &terminating}},
			},
		},
		{
			Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"2001:db8::1", "10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			},
		},
	}

	got, err := EndpointAddresses(slices, "grpc")
	if err != nil {
		t.Fatalf("EndpointAddresses returned error: %v", err)
	}
	want := []string{"10.0.0.2:8080", "10.0.0.3:8080", "[2001:db8::1]:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

func TestEndpointAddressesSkipsSlicesWithoutConfiguredPort(t *testing.T) {
	ready := true
	otherName := "metrics"
	port := int32(9090)
	slices := []discoveryv1.EndpointSlice{{
		Ports:     []discoveryv1.EndpointPort{{Name: &otherName, Port: &port}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}}

	got, err := EndpointAddresses(slices, "grpc")
	if err != nil {
		t.Fatalf("EndpointAddresses returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("addresses = %v, want empty successful snapshot", got)
	}
}
