package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/CodeRushOJ/croj-judging-server/internal/external"
)

type RequestAuthenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}

type Server struct {
	authenticator    RequestAuthenticator
	capabilities     Capabilities
	jobs             JobService
	jobWriteQuota    external.Quota
	jobWriteLimit    external.QuotaLimit
	bundles          BundleApplication
	bundleWriteQuota external.Quota
	bundleWriteLimit external.QuotaLimit
	bundleUploads    chan struct{}
}

func WithJobWriteQuota(quota external.Quota, jobSubmitLimit external.QuotaLimit) ServerOption {
	return func(server *Server) error {
		if quota == nil {
			return fmt.Errorf("write quota is required")
		}
		if err := jobSubmitLimit.Validate(); err != nil {
			return err
		}
		server.jobWriteQuota = quota
		server.jobWriteLimit = jobSubmitLimit
		return nil
	}
}

type ServerOption func(*Server) error

func WithJobService(service JobService) ServerOption {
	return func(server *Server) error {
		if service == nil {
			return fmt.Errorf("job service is required")
		}
		server.jobs = service
		return nil
	}
}

func WithBundleUploadConcurrency(maximum int) ServerOption {
	return func(server *Server) error {
		if maximum < 1 || maximum > 1024 {
			return fmt.Errorf("bundle upload concurrency must be between 1 and 1024")
		}
		server.bundleUploads = make(chan struct{}, maximum)
		return nil
	}
}
func NewServer(authenticator RequestAuthenticator, capabilities Capabilities, options ...ServerOption) (*Server, error) {
	if authenticator == nil {
		return nil, fmt.Errorf("request authenticator is required")
	}
	var err error
	capabilities, err = normalizeCapabilities(capabilities)
	if err != nil {
		return nil, err
	}
	server := &Server{authenticator: authenticator, capabilities: capabilities}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("server option is required")
		}
		if err := option(server); err != nil {
			return nil, err
		}
	}
	if server.jobs != nil && server.jobWriteQuota == nil {
		return nil, fmt.Errorf("write quota is required when the job service is enabled")
	}
	if server.bundles != nil && server.bundleWriteQuota == nil {
		return nil, fmt.Errorf("bundle write quota is required when bundle uploads are enabled")
	}
	if server.bundles != nil && server.bundleWriteLimit.Capacity < capabilities.Limits.MaxBundleBytes {
		return nil, fmt.Errorf("bundle write quota capacity must cover the maximum bundle size")
	}
	if server.bundles != nil && server.bundleUploads == nil {
		server.bundleUploads = make(chan struct{}, 4)
	}
	return server, nil
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID := newRequestID()
	response.Header().Set("X-Request-Id", requestID)
	switch {
	case request.URL.Path == "/api/v1/capabilities":
		server.handleCapabilities(response, request, requestID)
	case server.bundles != nil && request.URL.Path == "/api/v1/bundles":
		server.serveBundleCollection(response, request, requestID)
	case server.bundles != nil && strings.HasPrefix(request.URL.Path, "/api/v1/bundles/"):
		server.serveBundleMetadata(response, request, requestID)
	case server.jobs != nil && (request.URL.Path == "/api/v1/judge-jobs" || strings.HasPrefix(request.URL.Path, "/api/v1/judge-jobs/")):
		server.handleJobs(response, request, requestID)
	default:
		writeProblem(response, problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested API resource does not exist.", requestID))
	}
}

func (server *Server) handleCapabilities(response http.ResponseWriter, request *http.Request, requestID string) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use GET for this resource.", requestID))
		return
	}
	_, authenticated := server.authenticate(response, request, requestID, ScopeCapabilitiesRead)
	if !authenticated {
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(server.capabilities)
}

func (server *Server) authenticate(response http.ResponseWriter, request *http.Request, requestID string, scope Scope) (Principal, bool) {
	principal, err := server.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, ErrAuthenticationUnavailable) {
			response.Header().Set("Retry-After", "5")
			writeProblem(response, problemFor(http.StatusServiceUnavailable, "authentication-unavailable", "Authentication temporarily unavailable", "Retry the request later.", requestID))
			return Principal{}, false
		}
		response.Header().Set("WWW-Authenticate", `Bearer realm="coderushoj-judge"`)
		writeProblem(response, problemFor(http.StatusUnauthorized, "unauthorized", "Authentication required", "Provide a valid active API key.", requestID))
		return Principal{}, false
	}
	if !principal.Has(scope) {
		writeProblem(response, problemFor(http.StatusForbidden, "insufficient-scope", "Insufficient scope", "The API key does not grant this operation.", requestID))
		return Principal{}, false
	}
	return principal, true
}

func newRequestID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failure is exceptional; retain correlation without exposing request data.
		return "unavailable"
	}
	return hex.EncodeToString(random[:])
}
