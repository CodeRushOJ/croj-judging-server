package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type JobStatus string

const (
	JobQueued    JobStatus = "QUEUED"
	JobRunning   JobStatus = "RUNNING"
	JobSucceeded JobStatus = "SUCCEEDED"
	JobFailed    JobStatus = "FAILED"
	JobCancelled JobStatus = "CANCELLED"
)

var (
	ErrJobNotFound         = errors.New("judge job not found")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with another request")
	ErrJobInvalid          = errors.New("judge job request is invalid")
	ErrJobUnavailable      = errors.New("judge job service is unavailable")
	jobIDPattern           = regexp.MustCompile(`^[a-z2-7]{26}$`)
)

type SubmitJobCommand struct {
	BundleID        string `json:"bundleId"`
	Language        string `json:"language"`
	SourceCode      string `json:"sourceCode"`
	StopOnFailure   bool   `json:"stopOnFailure"`
	CallbackID      string `json:"callbackId,omitempty"`
	ClientReference string `json:"clientReference,omitempty"`
}

type CaseResultView struct {
	CaseID      string `json:"caseId"`
	Verdict     string `json:"verdict"`
	TimeMillis  int64  `json:"timeMillis"`
	MemoryBytes int64  `json:"memoryBytes"`
}

type JobResultView struct {
	Verdict       string           `json:"verdict"`
	CompileStatus string           `json:"compileStatus"`
	TimeMillis    int64            `json:"timeMillis"`
	MemoryBytes   int64            `json:"memoryBytes"`
	Cases         []CaseResultView `json:"cases"`
}

type JobView struct {
	JobID           string         `json:"jobId"`
	Status          JobStatus      `json:"status"`
	StatusURL       string         `json:"statusUrl,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	ClientReference string         `json:"clientReference,omitempty"`
	Result          *JobResultView `json:"result,omitempty"`
}

type JobListQuery struct {
	Cursor string
	Limit  int
	Status JobStatus
}

type JobListPage struct {
	Items      []JobView `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// JobAdmission must be invoked exactly once only after an implementation has
// serialized the Idempotency-Key and established that the request creates a
// new job. Replays and conflicts must return without invoking admission.
type JobAdmission func() error

type JobService interface {
	Submit(context.Context, string, string, SubmitJobCommand, JobAdmission) (JobView, bool, error)
	List(context.Context, string, JobListQuery) (JobListPage, error)
	Get(context.Context, string, string) (JobView, error)
	Cancel(context.Context, string, string) (JobView, error)
}

func (server *Server) handleJobs(response http.ResponseWriter, request *http.Request, requestID string) {
	if request.URL.Path == "/api/v1/judge-jobs" {
		server.handleJobCollection(response, request, requestID)
		return
	}
	relative := strings.TrimPrefix(request.URL.Path, "/api/v1/judge-jobs/")
	segments := strings.Split(relative, "/")
	if len(segments) == 1 && jobIDPattern.MatchString(segments[0]) {
		server.handleJobGet(response, request, requestID, segments[0])
		return
	}
	if len(segments) == 2 && segments[1] == "cancel" && jobIDPattern.MatchString(segments[0]) {
		server.handleJobCancel(response, request, requestID, segments[0])
		return
	}
	writeProblem(response, problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested judge job does not exist.", requestID))
}

func (server *Server) handleJobCollection(response http.ResponseWriter, request *http.Request, requestID string) {
	switch request.Method {
	case http.MethodPost:
		server.handleJobSubmit(response, request, requestID)
	case http.MethodGet:
		server.handleJobList(response, request, requestID)
	default:
		response.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use POST to submit a judge job.", requestID))
	}
}

func (server *Server) handleJobSubmit(response http.ResponseWriter, request *http.Request, requestID string) {
	principal, authenticated := server.authenticate(response, request, requestID, ScopeJobSubmit)
	if !authenticated {
		return
	}
	idempotencyValues := request.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 {
		writeProblem(response, problemFor(http.StatusBadRequest, "invalid-idempotency-key", "Invalid Idempotency-Key", "Provide exactly one Idempotency-Key header.", requestID))
		return
	}
	idempotencyKey := idempotencyValues[0]
	if err := external.ValidateIdempotencyKey(idempotencyKey); err != nil {
		writeProblem(response, problemFor(http.StatusBadRequest, "invalid-idempotency-key", "Invalid Idempotency-Key", "Provide 16 to 128 visible ASCII characters.", requestID))
		return
	}
	var command SubmitJobCommand
	if err := decodeStrictJSON(response, request, &command, maximumJobRequestBytes(server.capabilities.Limits.MaxSourceBytes)); err != nil {
		writeProblem(response, problemFor(http.StatusBadRequest, "invalid-json", "Invalid request body", "Provide one JSON object containing only documented fields.", requestID))
		return
	}
	var admissionOnce sync.Once
	var admissionErr error
	admit := func() error {
		admissionOnce.Do(func() {
			decision, err := server.jobWriteQuota.Allow(request.Context(), external.QuotaRequest{
				TenantID: principal.TenantID,
				Kind:     external.QuotaJudgeSubmit,
				Cost:     1,
				Limit:    server.jobWriteLimit,
			})
			if err != nil {
				if errors.Is(err, external.ErrQuotaUnavailable) {
					admissionErr = &jobQuotaAdmissionError{unavailable: true}
					return
				}
				admissionErr = err
				return
			}
			if !decision.Allowed {
				admissionErr = &jobQuotaAdmissionError{retryAfter: decision.RetryAfter}
			}
		})
		return admissionErr
	}
	view, replayed, err := server.jobs.Submit(request.Context(), principal.TenantID, idempotencyKey, command, admit)
	if err != nil {
		server.writeJobError(response, requestID, err)
		return
	}
	if view.StatusURL == "" {
		view.StatusURL = "/api/v1/judge-jobs/" + view.JobID
	}
	response.Header().Set("Location", view.StatusURL)
	if replayed {
		response.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(response, http.StatusAccepted, view)
}

func (server *Server) handleJobList(response http.ResponseWriter, request *http.Request, requestID string) {
	principal, authenticated := server.authenticate(response, request, requestID, ScopeJobRead)
	if !authenticated {
		return
	}
	query, err := parseJobListQuery(request)
	if err != nil {
		writeProblem(response, problemFor(http.StatusBadRequest, "invalid-list-query", "Invalid list query", "Use only cursor, limit (1-100), and a documented status.", requestID))
		return
	}
	page, err := server.jobs.List(request.Context(), principal.TenantID, query)
	if err != nil {
		server.writeJobError(response, requestID, err)
		return
	}
	if page.Items == nil {
		page.Items = []JobView{}
	}
	writeJSON(response, http.StatusOK, page)
}

func parseJobListQuery(request *http.Request) (JobListQuery, error) {
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return JobListQuery{}, ErrJobInvalid
	}
	for key, entries := range values {
		if key != "cursor" && key != "limit" && key != "status" || len(entries) != 1 {
			return JobListQuery{}, ErrJobInvalid
		}
	}
	query := JobListQuery{Cursor: values.Get("cursor"), Limit: 50}
	if len(query.Cursor) > 512 {
		return JobListQuery{}, ErrJobInvalid
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return JobListQuery{}, ErrJobInvalid
		}
		query.Limit = limit
	}
	if raw := values.Get("status"); raw != "" {
		query.Status = JobStatus(raw)
		switch query.Status {
		case JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled:
		default:
			return JobListQuery{}, ErrJobInvalid
		}
	}
	return query, nil
}

func maximumJobRequestBytes(maxSourceBytes int64) int64 {
	return maxSourceBytes*6 + (64 << 10)
}

func (server *Server) handleJobGet(response http.ResponseWriter, request *http.Request, requestID, jobID string) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use GET to read a judge job.", requestID))
		return
	}
	principal, authenticated := server.authenticate(response, request, requestID, ScopeJobRead)
	if !authenticated {
		return
	}
	view, err := server.jobs.Get(request.Context(), principal.TenantID, jobID)
	if err != nil {
		server.writeJobError(response, requestID, err)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (server *Server) handleJobCancel(response http.ResponseWriter, request *http.Request, requestID, jobID string) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use POST to cancel a judge job.", requestID))
		return
	}
	principal, authenticated := server.authenticate(response, request, requestID, ScopeJobCancel)
	if !authenticated {
		return
	}
	view, err := server.jobs.Cancel(request.Context(), principal.TenantID, jobID)
	if err != nil {
		server.writeJobError(response, requestID, err)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (server *Server) writeJobError(response http.ResponseWriter, requestID string, err error) {
	var quotaError *jobQuotaAdmissionError
	if errors.As(err, &quotaError) {
		if quotaError.unavailable {
			response.Header().Set("Retry-After", "5")
			writeProblem(response, problemFor(http.StatusServiceUnavailable, "quota-unavailable", "Quota temporarily unavailable", "Retry the request later.", requestID))
			return
		}
		retrySeconds := int64(math.Ceil(quotaError.retryAfter.Seconds()))
		if retrySeconds < 1 {
			retrySeconds = 1
		}
		response.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
		writeProblem(response, problemFor(http.StatusTooManyRequests, "quota-exceeded", "Quota exceeded", "Retry after the indicated delay.", requestID))
		return
	}
	switch {
	case errors.Is(err, ErrJobNotFound):
		writeProblem(response, problemFor(http.StatusNotFound, "job-not-found", "Judge job not found", "The requested judge job does not exist.", requestID))
	case errors.Is(err, ErrIdempotencyConflict):
		writeProblem(response, problemFor(http.StatusConflict, "idempotency-conflict", "Idempotency conflict", "The Idempotency-Key is already bound to a different request.", requestID))
	case errors.Is(err, ErrJobInvalid):
		writeProblem(response, problemFor(http.StatusUnprocessableEntity, "invalid-job", "Judge job is invalid", "The job could not be accepted under tenant policy.", requestID))
	case errors.Is(err, ErrJobUnavailable):
		response.Header().Set("Retry-After", "5")
		writeProblem(response, problemFor(http.StatusServiceUnavailable, "job-service-unavailable", "Judge job service unavailable", "Retry the request later.", requestID))
	default:
		writeProblem(response, problemFor(http.StatusInternalServerError, "internal-error", "Internal server error", "The request could not be completed.", requestID))
	}
}

type jobQuotaAdmissionError struct {
	unavailable bool
	retryAfter  time.Duration
}

func (failure *jobQuotaAdmissionError) Error() string {
	if failure != nil && failure.unavailable {
		return "job write quota is unavailable"
	}
	return "job write quota is exceeded"
}

func decodeStrictJSON(response http.ResponseWriter, request *http.Request, target any, maximum int64) error {
	request.Body = http.MaxBytesReader(response, request.Body, maximum)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
