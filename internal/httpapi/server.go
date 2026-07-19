package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type RequestAuthenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}

type Server struct {
	authenticator RequestAuthenticator
	capabilities  Capabilities
}

func NewServer(authenticator RequestAuthenticator, capabilities Capabilities) (*Server, error) {
	if authenticator == nil {
		return nil, fmt.Errorf("request authenticator is required")
	}
	if capabilities.APIVersion != "v1" || len(capabilities.Languages) == 0 {
		return nil, fmt.Errorf("v1 capabilities and at least one language are required")
	}
	return &Server{authenticator: authenticator, capabilities: capabilities}, nil
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID := newRequestID()
	response.Header().Set("X-Request-Id", requestID)
	if request.URL.Path != "/api/v1/capabilities" {
		writeProblem(response, problemFor(http.StatusNotFound, "not-found", "Resource not found", "The requested API resource does not exist.", requestID))
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeProblem(response, problemFor(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "Use GET for this resource.", requestID))
		return
	}
	principal, err := server.authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, ErrAuthenticationUnavailable) {
			response.Header().Set("Retry-After", "5")
			writeProblem(response, problemFor(http.StatusServiceUnavailable, "authentication-unavailable", "Authentication temporarily unavailable", "Retry the request later.", requestID))
			return
		}
		response.Header().Set("WWW-Authenticate", `Bearer realm="coderushoj-judge"`)
		writeProblem(response, problemFor(http.StatusUnauthorized, "unauthorized", "Authentication required", "Provide a valid active API key.", requestID))
		return
	}
	if !principal.Has(ScopeCapabilitiesRead) {
		writeProblem(response, problemFor(http.StatusForbidden, "insufficient-scope", "Insufficient scope", "The API key cannot read capabilities.", requestID))
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(server.capabilities)
}

func newRequestID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failure is exceptional; retain correlation without exposing request data.
		return "unavailable"
	}
	return hex.EncodeToString(random[:])
}
