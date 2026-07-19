package external

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeTerminalWebhookCompletedUsesStableRedactedBytes(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 34, 56, 789_123_456, time.FixedZone("offset", 8*60*60))
	result := DurableJobResult{
		Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", TimeMillis: 8, MemoryBytes: 1024,
		Cases: []DurableCaseResult{},
	}
	eventType, semantic, exact, err := EncodeTerminalWebhookEvent(TerminalWebhookEvent{
		EventID:    "ceirceirceirceirceirceirce",
		OccurredAt: now,
		Job: ExternalJobRecord{
			ExternalID: "deirceirceirceirceirceirce", TenantExternalID: "eeirceirceirceirceirceirce",
			ClientReference: "submission-1", Status: JobStatusSucceeded, Result: &result,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"eventId":"ceirceirceirceirceirceirce","eventType":"judge.job.completed","occurredAt":"2026-07-19T04:34:56.789Z","tenantId":"eeirceirceirceirceirceirce","jobId":"deirceirceirceirceirceirce","clientReference":"submission-1","status":"SUCCEEDED","result":{"verdict":"ACCEPTED","compileStatus":"SUCCEEDED","timeMillis":8,"memoryBytes":1024,"cases":[]}}`
	if eventType != "judge.job.completed" || string(semantic) != want || string(exact) != want {
		t.Fatalf("type=%q semantic=%s exact=%s", eventType, semantic, exact)
	}
	semantic[0] = '['
	if exact[0] != '{' {
		t.Fatal("semantic and exact payload share mutable storage")
	}
}

func TestEncodeTerminalWebhookFailedAndCancelledOmitSensitiveFields(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, status, failure, eventType string
	}{
		{"failed", string(JobStatusFailed), "SANDBOX_UNAVAILABLE", "judge.job.failed"},
		{"cancelled", string(JobStatusCancelled), "", "judge.job.cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			eventType, _, body, err := EncodeTerminalWebhookEvent(TerminalWebhookEvent{
				EventID: "ceirceirceirceirceirceirce", OccurredAt: now,
				Job: ExternalJobRecord{
					ExternalID: "deirceirceirceirceirceirce", TenantExternalID: "eeirceirceirceirceirceirce",
					Status: JobStatus(test.status), FailureCode: test.failure,
				},
			})
			if err != nil || eventType != test.eventType {
				t.Fatalf("type=%q error=%v", eventType, err)
			}
			encoded := string(body)
			if strings.Contains(encoded, `"result"`) || strings.Contains(encoded, "source") || strings.Contains(encoded, "object") || strings.Contains(encoded, "worker") || strings.Contains(encoded, "lease") {
				t.Fatalf("payload leaked or included result: %s", encoded)
			}
			if test.status == string(JobStatusFailed) && !strings.Contains(encoded, `"failureCode":"SANDBOX_UNAVAILABLE"`) {
				t.Fatalf("failed payload=%s", encoded)
			}
			if test.status == string(JobStatusCancelled) && strings.Contains(encoded, "failureCode") {
				t.Fatalf("cancelled payload=%s", encoded)
			}
		})
	}
}

func TestEncodeTerminalWebhookRejectsIncompleteOrNonterminalInput(t *testing.T) {
	valid := TerminalWebhookEvent{
		EventID: "ceirceirceirceirceirceirce", OccurredAt: time.Now().UTC(),
		Job: ExternalJobRecord{ExternalID: "deirceirceirceirceirceirce", TenantExternalID: "eeirceirceirceirceirceirce", Status: JobStatusCancelled},
	}
	for name, mutate := range map[string]func(*TerminalWebhookEvent){
		"invalid event":     func(event *TerminalWebhookEvent) { event.EventID = "bad" },
		"zero time":         func(event *TerminalWebhookEvent) { event.OccurredAt = time.Time{} },
		"invalid tenant":    func(event *TerminalWebhookEvent) { event.Job.TenantExternalID = "bad" },
		"invalid job":       func(event *TerminalWebhookEvent) { event.Job.ExternalID = "bad" },
		"queued":            func(event *TerminalWebhookEvent) { event.Job.Status = JobStatusQueued },
		"success no result": func(event *TerminalWebhookEvent) { event.Job.Status = JobStatusSucceeded },
		"failed no code":    func(event *TerminalWebhookEvent) { event.Job.Status = JobStatusFailed },
		"cancelled result": func(event *TerminalWebhookEvent) {
			event.Job.Result = &DurableJobResult{Verdict: "ACCEPTED", CompileStatus: "SUCCEEDED", Cases: []DurableCaseResult{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			event := valid
			mutate(&event)
			if _, _, _, err := EncodeTerminalWebhookEvent(event); err == nil {
				t.Fatal("invalid event encoded")
			}
		})
	}
}
