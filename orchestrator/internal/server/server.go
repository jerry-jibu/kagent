// Package server exposes the orchestrator's transport endpoints: healthz
// for k8s probes, the A2A push receiver for task terminal states
// (docs/designs/0001 D2), and minimal admin endpoints for verifying runtime
// state.
package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// PushReceiverPath is the path the A2A push receiver is mounted on. It
// mirrors internal/dispatch.PushReceiverPath (kept a literal here so the
// server package stays free of the dispatch dependency graph).
const PushReceiverPath = "/a2a/push"

// Readiness reports why the orchestrator is not serving yet (nil = ready).
type Readiness func() error

// PolicyView returns the currently served policy version, when it was
// loaded, and whether any policy is loaded at all.
type PolicyView func() (version string, loadedAt time.Time, ok bool)

// NewHandler builds the HTTP mux. A nil push receiver leaves the push path
// unmounted (the dispatch chain disabled — probing it answers 404).
func NewHandler(ready Readiness, policy PolicyView, push http.Handler) http.Handler {
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
	if push != nil {
		mux.Handle("POST "+PushReceiverPath, push)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
