package consumer

import (
	"context"
	"errors"
	"testing"

	"github.com/CodeRushOJ/croj-judging-server/internal/callback"
	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	rocketconsumer "github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

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
