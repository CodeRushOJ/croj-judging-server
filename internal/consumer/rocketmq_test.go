package consumer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	rocketconsumer "github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

type fakeNameServerResolver struct {
	addresses map[string][]string
	err       error
	lookups   []string
}

func (resolver *fakeNameServerResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	resolver.lookups = append(resolver.lookups, host)
	if resolver.err != nil {
		return nil, resolver.err
	}
	return resolver.addresses[host], nil
}

func TestResolveRocketMQNameServersExpandsEveryDNSAddressDeterministically(t *testing.T) {
	resolver := &fakeNameServerResolver{addresses: map[string][]string{
		"rocketmq.coderushoj.svc": {
			"2001:db8::2",
			"10.0.0.12",
			"10.0.0.11",
			"10.0.0.12",
		},
	}}

	nameservers, err := newRocketMQNameServerResolver(
		"rocketmq.coderushoj.svc:9876",
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	addresses := nameservers.Resolve()
	expected := []string{"10.0.0.11:9876", "10.0.0.12:9876", "[2001:db8::2]:9876"}
	if !reflect.DeepEqual(expected, addresses) {
		t.Fatalf("addresses = %v, want %v", addresses, expected)
	}
	if _, err := primitive.NewNamesrvAddr(addresses...); err != nil {
		t.Fatalf("resolved addresses are rejected by RocketMQ: %v", err)
	}
}

func TestResolveRocketMQNameServersPreservesIPInputsWithoutDNS(t *testing.T) {
	resolver := &fakeNameServerResolver{}
	nameservers, err := newRocketMQNameServerResolver(
		"10.0.0.9:9876;[2001:db8::9]:9876",
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	addresses := nameservers.Resolve()
	expected := []string{"10.0.0.9:9876", "[2001:db8::9]:9876"}
	if !reflect.DeepEqual(expected, addresses) {
		t.Fatalf("addresses = %v, want %v", addresses, expected)
	}
	if len(resolver.lookups) != 0 {
		t.Fatalf("IP literals unexpectedly used DNS: %v", resolver.lookups)
	}
}

func TestResolveRocketMQNameServersFailsClosedWithoutALastKnownGoodAnswer(t *testing.T) {
	for name, resolver := range map[string]*fakeNameServerResolver{
		"lookup error": {err: errors.New("DNS unavailable")},
		"empty answer": {addresses: map[string][]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			nameservers, err := newRocketMQNameServerResolver(
				"rocketmq.coderushoj.svc:9876",
				resolver,
			)
			if err != nil {
				t.Fatal(err)
			}
			if addresses := nameservers.Resolve(); len(addresses) != 0 {
				t.Fatalf("addresses = %v, want no unsafe fallback", addresses)
			}
		})
	}
}

func TestResolveRocketMQNameServersRetainsLastKnownGoodOnRefreshFailure(t *testing.T) {
	resolver := &fakeNameServerResolver{addresses: map[string][]string{
		"rocketmq.coderushoj.svc": {"10.0.0.11"},
	}}
	nameservers, err := newRocketMQNameServerResolver(
		"rocketmq.coderushoj.svc:9876",
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"10.0.0.11:9876"}
	if actual := nameservers.Resolve(); !reflect.DeepEqual(expected, actual) {
		t.Fatalf("first resolve = %v, want %v", actual, expected)
	}

	resolver.err = errors.New("temporary DNS failure")
	if actual := nameservers.Resolve(); !reflect.DeepEqual(expected, actual) {
		t.Fatalf("failed refresh = %v, want last-known-good %v", actual, expected)
	}
}

type fakeEventProcessor struct {
	event model.SubmissionRequested
	err   error
	calls int
}

func (processor *fakeEventProcessor) ProcessEvent(_ context.Context, event model.SubmissionRequested) error {
	processor.calls++
	processor.event = event
	return processor.err
}

func TestRocketMQConsumerAcknowledgesInvalidAndPermanentMessages(t *testing.T) {
	for name, test := range map[string]struct {
		body string
		err  error
	}{
		"invalid JSON": {"not-json", nil},
		"HTTP 409":     {validEventJSON(), callback.Permanent(errors.New("HTTP 409"))},
	} {
		t.Run(name, func(t *testing.T) {
			processor := &fakeEventProcessor{err: test.err}
			consumer := &RocketMQConsumer{processor: processor}
			result, err := consumer.handleMessage(context.Background(), &primitive.MessageExt{Message: primitive.Message{Body: []byte(test.body)}})
			if err != nil || result != rocketconsumer.ConsumeSuccess {
				t.Fatalf("handleMessage = %v, %v", result, err)
			}
		})
	}
}

func TestRocketMQConsumerRetriesTransientFailure(t *testing.T) {
	processor := &fakeEventProcessor{err: errors.New("rpc error: code = Unavailable")}
	consumer := &RocketMQConsumer{processor: processor}
	result, err := consumer.handleMessage(context.Background(), &primitive.MessageExt{Message: primitive.Message{Body: []byte(validEventJSON())}})
	if err != nil || result != rocketconsumer.ConsumeRetryLater {
		t.Fatalf("handleMessage = %v, %v", result, err)
	}
	if processor.calls != 1 || processor.event.EventID == "" {
		t.Fatalf("processor = %+v", processor)
	}
}

func validEventJSON() string {
	return `{"schemaVersion":1,"eventId":"50f75fdf-fdea-473f-a156-bf1ed60acf58","submissionId":99,"attemptNo":1,"problemId":42,"userId":7,"language":"java17"}`
}
