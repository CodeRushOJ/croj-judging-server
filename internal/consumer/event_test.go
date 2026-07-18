package consumer

import "testing"

func TestDecodeSubmissionRequestedV1(t *testing.T) {
	event, err := DecodeSubmissionRequested([]byte(`{
  "schemaVersion": 1,
  "eventId": "50f75fdf-fdea-473f-a156-bf1ed60acf58",
  "submissionId": 99,
  "attemptNo": 1,
  "problemId": 42,
  "userId": 7,
  "language": "java17"
}`))
	if err != nil {
		t.Fatalf("DecodeSubmissionRequested: %v", err)
	}
	if event.EventID != "50f75fdf-fdea-473f-a156-bf1ed60acf58" || event.SubmissionID != 99 || event.AttemptNo != 1 {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.DeduplicationKey() != "50f75fdf-fdea-473f-a156-bf1ed60acf58/99/1" {
		t.Fatalf("deduplication key = %q", event.DeduplicationKey())
	}
}

func TestDecodeSubmissionRequestedRejectsInvalidContract(t *testing.T) {
	tests := map[string]string{
		"unsupported version": `{"schemaVersion":2,"eventId":"evt","submissionId":1,"attemptNo":1,"problemId":1,"userId":1,"language":"go"}`,
		"missing event id":    `{"schemaVersion":1,"submissionId":1,"attemptNo":1,"problemId":1,"userId":1,"language":"go"}`,
		"invalid event id":    `{"schemaVersion":1,"eventId":"evt","submissionId":1,"attemptNo":1,"problemId":1,"userId":1,"language":"go"}`,
		"invalid submission":  `{"schemaVersion":1,"eventId":"evt","submissionId":0,"attemptNo":1,"problemId":1,"userId":1,"language":"go"}`,
		"invalid attempt":     `{"schemaVersion":1,"eventId":"evt","submissionId":1,"attemptNo":0,"problemId":1,"userId":1,"language":"go"}`,
		"unknown field":       `{"schemaVersion":1,"eventId":"evt","submissionId":1,"attemptNo":1,"problemId":1,"userId":1,"language":"go","source":"secret"}`,
		"trailing document":   `{"schemaVersion":1,"eventId":"evt","submissionId":1,"attemptNo":1,"problemId":1,"userId":1,"language":"go"}{}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSubmissionRequested([]byte(body)); err == nil {
				t.Fatal("expected contract error")
			}
		})
	}
}
