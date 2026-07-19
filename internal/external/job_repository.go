package external

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrExternalJobNotFound    = errors.New("external judge job not found")
	ErrExternalJobConflict    = errors.New("external judge job idempotency conflict")
	ErrExternalJobInvalid     = errors.New("external judge job is invalid")
	ErrExternalJobUnavailable = errors.New("external judge job repository is unavailable")
	ErrQueuedQuotaExceeded    = errors.New("tenant queued job quota exceeded")
	ErrInvalidJobCursor       = errors.New("invalid external judge job cursor")
	ErrSourceObjectExists     = errors.New("encrypted source object already exists")
)

type SourceObjectStore interface {
	// Create must be atomic and must return ErrSourceObjectExists instead of
	// overwriting an existing key. This makes a random-ID collision harmless.
	Create(context.Context, string, []byte) error
	Get(context.Context, string, int64) ([]byte, error)
	Delete(context.Context, string) error
}

type SourceObjectMetadata struct {
	ExternalID string
	ObjectKey  string
	SHA256     []byte
	SizeBytes  int64
	KeyVersion uint16
	Nonce      []byte
}

func (SourceObjectMetadata) String() string   { return "[REDACTED SOURCE OBJECT]" }
func (SourceObjectMetadata) GoString() string { return "[REDACTED SOURCE OBJECT]" }

type ExternalJobRecord struct {
	InternalID       uint64
	ExternalID       string
	TenantExternalID string
	BundleExternalID string
	Source           SourceObjectMetadata
	CallbackID       string
	Status           JobStatus
	Language         string
	StopOnFailure    bool
	ClientReference  string
	AttemptNo        uint32
	WorkerID         string
	LeaseUntil       *time.Time
	CancelRequested  *time.Time
	Result           *DurableJobResult
	FailureCode      string
	CreatedAt        time.Time
	StartedAt        *time.Time
	CompletedAt      *time.Time
}

type SubmitJobResult struct {
	Job      ExternalJobRecord
	Replayed bool
}

type JobListOptions struct {
	Cursor string
	Limit  int
	Status JobStatus
}

type JobListResult struct {
	Jobs       []ExternalJobRecord
	NextCursor string
}

type WorkerJobClaim struct {
	Job        ExternalJobRecord
	WorkerID   string
	AttemptNo  uint32
	LeaseToken []byte
	LeaseUntil time.Time
}

func (claim WorkerJobClaim) String() string {
	return fmt.Sprintf("WorkerJobClaim{JobID:%s Status:%s WorkerID:%s AttemptNo:%d LeaseUntil:%s}",
		claim.Job.ExternalID, claim.Job.Status, claim.WorkerID, claim.AttemptNo, claim.LeaseUntil.UTC().Format(time.RFC3339Nano))
}

func (claim WorkerJobClaim) GoString() string { return claim.String() }

type InfrastructureFailure struct {
	Code       string
	RetryDelay time.Duration
}

type FailureDisposition string

const (
	FailureRequeued  FailureDisposition = "REQUEUED"
	FailureTerminal  FailureDisposition = "TERMINAL"
	FailureCancelled FailureDisposition = "CANCELLED"
)

type JobCursor struct {
	TenantID   string
	Status     JobStatus
	CreatedAt  time.Time
	InternalID uint64
}

type jobCursorPayload struct {
	Version    int       `json:"v"`
	TenantID   string    `json:"t"`
	Status     JobStatus `json:"s,omitempty"`
	CreatedMS  int64     `json:"c"`
	InternalID uint64    `json:"i"`
}

type JobCursorCodec struct{ key []byte }

func NewJobCursorCodec(key []byte) (*JobCursorCodec, error) {
	if len(key) < sha256.Size {
		return nil, fmt.Errorf("job cursor HMAC key must contain at least 256 bits")
	}
	return &JobCursorCodec{key: append([]byte(nil), key...)}, nil
}

func (codec *JobCursorCodec) Encode(cursor JobCursor) (string, error) {
	if codec == nil || len(codec.key) < sha256.Size || !validJobCursor(cursor) {
		return "", ErrInvalidJobCursor
	}
	payload, err := json.Marshal(jobCursorPayload{
		Version: 1, TenantID: cursor.TenantID, Status: cursor.Status,
		CreatedMS: cursor.CreatedAt.UTC().UnixMilli(), InternalID: cursor.InternalID,
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode payload", ErrInvalidJobCursor)
	}
	signature := hmac.New(sha256.New, codec.key)
	_, _ = signature.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), nil
}

func (codec *JobCursorCodec) Decode(encoded, tenantID string, status JobStatus) (JobCursor, error) {
	if codec == nil || len(codec.key) < sha256.Size || len(encoded) == 0 || len(encoded) > 512 {
		return JobCursor{}, ErrInvalidJobCursor
	}
	parts := strings.Split(encoded, ".")
	if len(parts) != 2 {
		return JobCursor{}, ErrInvalidJobCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return JobCursor{}, ErrInvalidJobCursor
	}
	providedSignature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JobCursor{}, ErrInvalidJobCursor
	}
	expectedSignature := hmac.New(sha256.New, codec.key)
	_, _ = expectedSignature.Write(payload)
	if !hmac.Equal(providedSignature, expectedSignature.Sum(nil)) {
		return JobCursor{}, ErrInvalidJobCursor
	}
	var decoded jobCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || decoded.Version != 1 {
		return JobCursor{}, ErrInvalidJobCursor
	}
	cursor := JobCursor{
		TenantID: decoded.TenantID, Status: decoded.Status,
		CreatedAt: time.UnixMilli(decoded.CreatedMS).UTC(), InternalID: decoded.InternalID,
	}
	if cursor.TenantID != tenantID || cursor.Status != status || !validJobCursor(cursor) {
		return JobCursor{}, ErrInvalidJobCursor
	}
	return cursor, nil
}

func validJobCursor(cursor JobCursor) bool {
	if !externalIDPattern.MatchString(cursor.TenantID) || cursor.CreatedAt.IsZero() || cursor.InternalID == 0 {
		return false
	}
	switch cursor.Status {
	case "", JobStatusQueued, JobStatusRunning, JobStatusSucceeded, JobStatusFailed, JobStatusCancelled:
		return true
	default:
		return false
	}
}

func SourceObjectKey(tenantID, sourceID string) (string, error) {
	if !externalIDPattern.MatchString(tenantID) || !externalIDPattern.MatchString(sourceID) {
		return "", fmt.Errorf("tenant or source object ID is invalid")
	}
	return "external/" + tenantID + "/sources/" + sourceID + ".bin", nil
}
