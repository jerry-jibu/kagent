// Package server exposes the orchestrator's transport endpoints: healthz
// for k8s probes and minimal admin endpoints for verifying runtime state.
package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// Readiness reports why the orchestrator is not serving yet (nil = ready).
type Readiness func() error

// PolicyView returns the currently served policy version, when it was
// loaded, and whether any policy is loaded at all.
type PolicyView func() (version string, loadedAt time.Time, ok bool)

// NewHandler builds the HTTP mux.
func NewHandler(ready Readiness, policy PolicyView) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"status": "not-ready", "reason": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /admin/policy", func(w http.ResponseWriter, r *http.Request) {
		version, loadedAt, ok := policy()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"reason": "no policy loaded"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version":  version,
			"loadedAt": loadedAt,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
