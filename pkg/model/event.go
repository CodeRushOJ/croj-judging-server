package model

import "fmt"

type SubmissionRequested struct {
	SchemaVersion int    `json:"schemaVersion"`
	EventID       string `json:"eventId"`
	SubmissionID  int64  `json:"submissionId"`
	AttemptNo     int    `json:"attemptNo"`
	ProblemID     int64  `json:"problemId"`
	UserID        int64  `json:"userId"`
	Language      string `json:"language"`
}

func (event SubmissionRequested) DeduplicationKey() string {
	return fmt.Sprintf("%s/%d/%d", event.EventID, event.SubmissionID, event.AttemptNo)
}
