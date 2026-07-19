package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type BundleApplication interface {
	Upload(context.Context, string, string, io.Reader) (external.BundleMetadata, bool, error)
	Get(context.Context, string, string) (external.BundleMetadata, error)
}

func WithBundleApplication(application BundleApplication) ServerOption {
	return func(server *Server) error {
		if application == nil {
			return errors.New("bundle application is required")
		}
		server.bundles = application
		return nil
	}
}

func (server *Server) serveBundleCollection(response http.ResponseWriter, request *http.Request, requestID string) {
	if server.bundles == nil {
		writeProblem(response, problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested API resource does not exist.", requestID))
		return
	}
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use POST for this resource.", requestID))
		return
	}
	principal, ok := server.authenticate(response, request, requestID, ScopeBundleWrite)
	if !ok {
		return
	}
	idempotencyKeys := request.Header.Values("Idempotency-Key")
	if request.URL.RawQuery != "" || len(idempotencyKeys) != 1 || idempotencyKeys[0] == "" {
		writeBundleProblem(response, requestID, external.ErrInvalidIdempotency)
		return
	}
	const multipartEnvelopeBytes = int64(1 << 20)
	maxBundleBytes := server.capabilities.Limits.MaxBundleBytes
	if maxBundleBytes <= 0 || maxBundleBytes > math.MaxInt64-multipartEnvelopeBytes {
		writeBundleProblem(response, requestID, errors.New("bundle upload limit is unavailable"))
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, maxBundleBytes+multipartEnvelopeBytes)
	multipartReader, err := request.MultipartReader()
	if err != nil {
		writeBundleProblem(response, requestID, external.ErrInvalidBundle)
		return
	}
	part, err := multipartReader.NextPart()
	if err != nil || part.FormName() != "bundle" || part.FileName() == "" {
		if part != nil {
			_ = part.Close()
		}
		writeBundleProblem(response, requestID, external.ErrInvalidBundle)
		return
	}
	reader := &singleMultipartFile{part: part, multipart: multipartReader}
	metadata, replay, err := server.bundles.Upload(request.Context(), principal.TenantID, idempotencyKeys[0], reader)
	_ = part.Close()
	if err != nil {
		_ = request.Body.Close()
		writeBundleProblem(response, requestID, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Location", "/api/v1/bundles/"+metadata.BundleID)
	response.WriteHeader(status)
	_ = encodeJSON(response, metadata)
}

func (server *Server) serveBundleMetadata(response http.ResponseWriter, request *http.Request, requestID string) {
	if server.bundles == nil {
		writeProblem(response, problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested API resource does not exist.", requestID))
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use GET for this resource.", requestID))
		return
	}
	principal, ok := server.authenticate(response, request, requestID, ScopeBundleRead)
	if !ok {
		return
	}
	bundleID := strings.TrimPrefix(request.URL.Path, "/api/v1/bundles/")
	if bundleID == "" || strings.Contains(bundleID, "/") || request.URL.RawQuery != "" {
		writeBundleProblem(response, requestID, external.ErrBundleNotFound)
		return
	}
	metadata, err := server.bundles.Get(request.Context(), principal.TenantID, bundleID)
	if err != nil {
		writeBundleProblem(response, requestID, err)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_ = encodeJSON(response, metadata)
}

var errUnexpectedMultipartPart = errors.New("multipart request must contain exactly one bundle file")

type singleMultipartFile struct {
	part      *multipart.Part
	multipart *multipart.Reader
	finished  bool
}

func (reader *singleMultipartFile) Read(buffer []byte) (int, error) {
	if reader.finished {
		return 0, io.EOF
	}
	count, err := reader.part.Read(buffer)
	if !errors.Is(err, io.EOF) {
		return count, err
	}
	reader.finished = true
	_ = reader.part.Close()
	next, nextErr := reader.multipart.NextPart()
	if next != nil {
		return count, errUnexpectedMultipartPart
	}
	if !errors.Is(nextErr, io.EOF) {
		return count, errUnexpectedMultipartPart
	}
	return count, io.EOF
}

func writeBundleProblem(response http.ResponseWriter, requestID string, err error) {
	problem := problemFor(http.StatusServiceUnavailable, "bundle-unavailable", "Bundle service unavailable", "The bundle operation could not be completed.", requestID)
	var maxBytesError *http.MaxBytesError
	switch {
	case errors.Is(err, external.ErrBundleTooLarge), errors.As(err, &maxBytesError):
		problem = problemFor(http.StatusRequestEntityTooLarge, "bundle-too-large", "Bundle too large", "The bundle exceeds the configured upload limit.", requestID)
	case errors.Is(err, external.ErrInvalidBundle), errors.Is(err, external.ErrInvalidIdempotency), errors.Is(err, errUnexpectedMultipartPart):
		problem = problemFor(http.StatusBadRequest, "invalid-bundle", "Invalid bundle upload", "Provide one valid immutable bundle ZIP and an Idempotency-Key.", requestID)
	case errors.Is(err, external.ErrIdempotencyConflict):
		problem = problemFor(http.StatusConflict, "idempotency-conflict", "Idempotency conflict", "The Idempotency-Key was already used for different content.", requestID)
	case errors.Is(err, external.ErrBundleNotFound):
		problem = problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested API resource does not exist.", requestID)
	case errors.Is(err, external.ErrBundlePublishing):
		response.Header().Set("Retry-After", "2")
		problem = problemFor(http.StatusServiceUnavailable, "bundle-publishing", "Bundle publication in progress", "Retry this idempotent upload later.", requestID)
	}
	writeProblem(response, problem)
}

func encodeJSON(writer io.Writer, value any) error {
	return json.NewEncoder(writer).Encode(value)
}
