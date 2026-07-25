package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Status string

const (
	StatusAccepted            Status = "ACCEPTED"
	StatusCompileError        Status = "COMPILE_ERROR"
	StatusWrongAnswer         Status = "WRONG_ANSWER"
	StatusTimeLimitExceeded   Status = "TIME_LIMIT_EXCEEDED"
	StatusMemoryLimitExceeded Status = "MEMORY_LIMIT_EXCEEDED"
	StatusRuntimeError        Status = "RUNTIME_ERROR"
	StatusOutputLimitExceeded Status = "OUTPUT_LIMIT_EXCEEDED"
	StatusSystemError         Status = "SYSTEM_ERROR"
)

type Result struct {
	ResultID       string `json:"resultId"`
	SubmissionID   int64  `json:"submissionId"`
	AttemptNo      int    `json:"attemptNo"`
	Status         Status `json:"status"`
	ExitCode       int    `json:"exitCode"`
	TimeUsedMillis int    `json:"timeUsedMillis"`
	MemoryUsedKB   int    `json:"memoryUsedKb"`
	Score          *int   `json:"score,omitempty"`
	TotalScore     *int   `json:"totalScore,omitempty"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	CompileError   string `json:"compileError"`
}

type Disposition string

const (
	DispositionApplied   Disposition = "APPLIED"
	DispositionDuplicate Disposition = "DUPLICATE"
)

type Client struct {
	endpoint     string
	serviceToken string
	timeout      time.Duration
	httpClient   *http.Client
}

func NewClient(baseURL, serviceToken string, timeout time.Duration, httpClient *http.Client) (*Client, error) {
	normalizedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(normalizedBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("BACKEND_INTERNAL_URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("BACKEND_INTERNAL_URL must not contain userinfo, query, fragment, or opaque data")
	}
	if strings.TrimRight(parsed.Path, "/") != "/api" {
		return nil, fmt.Errorf("BACKEND_INTERNAL_URL must include the /api context path")
	}
	if len([]byte(serviceToken)) < 32 {
		return nil, fmt.Errorf("JUDGE_RESULT_SERVICE_TOKEN must contain at least 32 bytes")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("judge result callback timeout must be positive")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientWithoutRedirects := *httpClient
	clientWithoutRedirects.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		endpoint:     normalizedBaseURL + "/internal/v1/judge-results",
		serviceToken: serviceToken,
		timeout:      timeout,
		httpClient:   &clientWithoutRedirects,
	}, nil
}

func (client *Client) Publish(ctx context.Context, result Result) (Disposition, error) {
	if err := validateResult(result); err != nil {
		return "", Permanent(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		return "", Permanent(fmt.Errorf("encode judge result: %w", err))
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", Permanent(fmt.Errorf("create judge result callback: %w", err))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CROJ-Service-Token", client.serviceToken)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("publish judge result: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("judge result callback returned HTTP %d", response.StatusCode)
		if response.StatusCode >= 400 && response.StatusCode < 500 && !isRetryableStatus(response.StatusCode) {
			return "", Permanent(err)
		}
		return "", err
	}

	var envelope struct {
		Code    int  `json:"code"`
		Success bool `json:"success"`
		Data    struct {
			Disposition Disposition `json:"disposition"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&envelope); err != nil {
		return "", fmt.Errorf("decode judge result callback: %w", err)
	}
	if envelope.Code != 20000 || !envelope.Success ||
		(envelope.Data.Disposition != DispositionApplied && envelope.Data.Disposition != DispositionDuplicate) {
		return "", fmt.Errorf("judge result callback returned an invalid success envelope")
	}
	return envelope.Data.Disposition, nil
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func validateResult(result Result) error {
	if strings.TrimSpace(result.ResultID) == "" || len(result.ResultID) > 128 {
		return fmt.Errorf("resultId must contain 1..128 bytes")
	}
	if result.SubmissionID <= 0 || result.AttemptNo <= 0 {
		return fmt.Errorf("submissionId and attemptNo must be positive")
	}
	if result.TimeUsedMillis < 0 || result.TimeUsedMillis > 172_800_000 || result.MemoryUsedKB < 0 || result.MemoryUsedKB > 2_147_483_647 {
		return fmt.Errorf("judge result metrics are outside the callback contract")
	}
	if (result.Score == nil) != (result.TotalScore == nil) {
		return fmt.Errorf("OI score and totalScore must be present together")
	}
	if result.Score != nil && (*result.TotalScore <= 0 || *result.TotalScore > 1_000_000_000 ||
		*result.Score < 0 || *result.Score > *result.TotalScore) {
		return fmt.Errorf("OI score is outside the callback contract")
	}
	if result.Score != nil {
		fullScore := *result.Score == *result.TotalScore
		switch {
		case result.Status == StatusCompileError && *result.Score != 0:
			return fmt.Errorf("OI COMPILE_ERROR requires score=0")
		case result.Status == StatusCompileError:
		case fullScore && result.Status != StatusAccepted:
			return fmt.Errorf("OI full score requires ACCEPTED")
		case !fullScore && result.Status != StatusWrongAnswer:
			return fmt.Errorf("OI partial score requires WRONG_ANSWER")
		}
	}
	if UTF16Len(result.Stdout) > 65_536 || UTF16Len(result.Stderr) > 65_536 || UTF16Len(result.CompileError) > 32_768 {
		return fmt.Errorf("judge result output exceeds the callback contract")
	}
	switch result.Status {
	case StatusAccepted, StatusCompileError, StatusWrongAnswer, StatusTimeLimitExceeded,
		StatusMemoryLimitExceeded, StatusRuntimeError, StatusOutputLimitExceeded, StatusSystemError:
	default:
		return fmt.Errorf("unsupported judge result status %q", result.Status)
	}
	if result.Status == StatusAccepted && result.ExitCode != 0 {
		return fmt.Errorf("ACCEPTED requires exitCode=0")
	}
	if result.Status == StatusCompileError && strings.TrimSpace(result.CompileError) == "" {
		return fmt.Errorf("COMPILE_ERROR requires compileError")
	}
	return nil
}

type permanentError struct{ error }

func (err permanentError) Unwrap() error { return err.error }

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{error: err}
}

func IsPermanent(err error) bool {
	var target permanentError
	return errors.As(err, &target)
}
