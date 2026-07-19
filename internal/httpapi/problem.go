package httpapi

import (
	"encoding/json"
	"net/http"
)

type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"requestId"`
}

func writeProblem(response http.ResponseWriter, problem Problem) {
	response.Header().Set("Content-Type", "application/problem+json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(problem.Status)
	_ = json.NewEncoder(response).Encode(problem)
}

func problemFor(status int, kind, title, detail, requestID string) Problem {
	return Problem{
		Type:      "https://coderushoj.dev/problems/" + kind,
		Title:     title,
		Status:    status,
		Detail:    detail,
		RequestID: requestID,
	}
}
