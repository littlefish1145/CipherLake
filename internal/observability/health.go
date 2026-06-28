package observability

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthChecker is a function that checks the health of a dependency.
// It returns true if the dependency is healthy, false otherwise.
type HealthChecker func() bool

// HealthHandler manages health check endpoints.
type HealthHandler struct {
	checks map[string]HealthChecker
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{
		checks: make(map[string]HealthChecker),
	}
}

// RegisterCheck registers a health check with the given name.
func (h *HealthHandler) RegisterCheck(name string, checker HealthChecker) {
	h.checks[name] = checker
}

// HealthzHandler returns an HTTP handler for the /healthz endpoint.
// It indicates that the process is alive.
func (h *HealthHandler) HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := healthResponse{
			Status:    "ok",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Checks:    map[string]string{"process": "alive"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}

// ReadyzHandler returns an HTTP handler for the /readyz endpoint.
// It checks that all dependencies are ready.
func (h *HealthHandler) ReadyzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		checks := make(map[string]string)
		allReady := true

		for name, checker := range h.checks {
			if checker() {
				checks[name] = "ok"
			} else {
				checks[name] = "unavailable"
				allReady = false
			}
		}

		status := "ok"
		code := http.StatusOK
		if !allReady {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}

		resp := healthResponse{
			Status:    status,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Checks:    checks,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(resp)
	}
}

// LivezHandler returns an HTTP handler for the /livez endpoint.
// It is a lightweight liveness probe for Kubernetes.
func (h *HealthHandler) LivezHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := healthResponse{
			Status:    "ok",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Checks:    map[string]string{"process": "alive"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}

type healthResponse struct {
	Status    string            `json:"status"`
	Timestamp string            `json:"timestamp"`
	Checks    map[string]string `json:"checks"`
}
