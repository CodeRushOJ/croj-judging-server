package consumer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/pkg/model"
	"github.com/google/uuid"
)

func DecodeSubmissionRequested(body []byte) (model.SubmissionRequested, error) {
	var event model.SubmissionRequested
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return event, fmt.Errorf("decode SubmissionRequested: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return event, fmt.Errorf("SubmissionRequested must contain exactly one JSON document")
	}
	if event.SchemaVersion != 1 {
		return event, fmt.Errorf("unsupported SubmissionRequested schemaVersion %d", event.SchemaVersion)
	}
	if event.EventID = strings.TrimSpace(event.EventID); event.EventID == "" || len(event.EventID) > 128 {
		return event, fmt.Errorf("SubmissionRequested eventId must contain 1..128 bytes")
	}
	if _, err := uuid.Parse(event.EventID); err != nil {
		return event, fmt.Errorf("SubmissionRequested eventId must be a UUID: %w", err)
	}
	if event.SubmissionID <= 0 || event.AttemptNo <= 0 || event.ProblemID <= 0 || event.UserID <= 0 {
		return event, fmt.Errorf("SubmissionRequested identifiers and attemptNo must be positive")
	}
	if event.Language = strings.TrimSpace(event.Language); event.Language == "" || len(event.Language) > 20 {
		return event, fmt.Errorf("SubmissionRequested language must contain 1..20 bytes")
	}
	return event, nil
}
