// Package server exposes the orchestrator's transport endpoints: healthz
// for k8s probes, the A2A push receiver for task terminal states
// (docs/designs 0001 D2), the minimal admin surface for the needs-attention
// loop (T8: list / retry / stop), and admin endpoints for verifying runtime
// state.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
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

// AdminService is the minimal admin surface (T8 issue #9): list the
// needs-attention tickets, retry a held ticket, stop live work. The store
// implements it; every mutating action is audited inside its transaction.
// The server only maps wire to domain and back — it holds no state.
type AdminService interface {
	NeedsAttentionTickets(ctx context.Context) ([]store.AttentionTicket, error)
	RetryTicket(ctx context.Context, ticketID, actor string) (store.Assignment, error)
	StopTicket(ctx context.Context, ticketID, actor string) error
}

// NewHandler builds the HTTP mux. A nil push receiver leaves the push path
// unmounted (the dispatch chain disabled — probing it answers 404); a nil
// admin service leaves the admin routes unmounted the same way.
func NewHandler(ready Readiness, policy PolicyView, push http.Handler, admin AdminService) http.Handler {
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
	if admin != nil {
		mux.HandleFunc("GET /admin/needs-attention", func(w http.ResponseWriter, r *http.Request) {
			tickets, err := admin.NeedsAttentionTickets(r.Context())
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]string{"reason": "failed to list needs-attention tickets"})
				return
			}
			rows := make([]map[string]any, 0, len(tickets))
			for _, tk := range tickets {
				rows = append(rows, attentionRow(tk))
			}
			writeJSON(w, http.StatusOK, map[string]any{"tickets": rows})
		})
		mux.HandleFunc("POST /admin/tickets/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
			minted, err := admin.RetryTicket(r.Context(), r.PathValue("id"), actorOf(r))
			switch {
			case err == nil:
				writeJSON(w, http.StatusOK, map[string]any{
					"ticket_id":     r.PathValue("id"),
					"ticket_status": store.TicketStatusRunning,
					"assignment_id": minted.ID,
				})
			case errors.Is(err, store.ErrTicketNotFound):
				writeAdminError(w, http.StatusNotFound, err)
			case errors.Is(err, store.ErrTicketNotHeld):
				writeAdminError(w, http.StatusConflict, err)
			default:
				writeAdminError(w, http.StatusInternalServerError, err)
			}
		})
		mux.HandleFunc("POST /admin/tickets/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
			err := admin.StopTicket(r.Context(), r.PathValue("id"), actorOf(r))
			switch {
			case err == nil:
				writeJSON(w, http.StatusOK, map[string]any{
					"ticket_id":     r.PathValue("id"),
					"ticket_status": store.TicketStatusFailed,
				})
			case errors.Is(err, store.ErrTicketNotFound):
				writeAdminError(w, http.StatusNotFound, err)
			case errors.Is(err, store.ErrTicketTerminal):
				writeAdminError(w, http.StatusConflict, err)
			default:
				writeAdminError(w, http.StatusInternalServerError, err)
			}
		})
	}
	return mux
}

// attentionRow shapes one admin list row: the ticket, its latest assignment,
// the waitsForPerson flag and the last audit event — the context a person
// needs to decide (docs/designs 0001 §3 Attention).
func attentionRow(tk store.AttentionTicket) map[string]any {
	assignment := map[string]any{
		"id":     tk.LatestAssignment.ID,
		"status": tk.LatestAssignment.Status,
		"report": tk.LatestAssignment.Report,
	}
	return map[string]any{
		"id":                tk.Ticket.ID,
		"status":            tk.Ticket.Status,
		"thread_source":     tk.Ticket.ThreadSource,
		"thread_key":        tk.Ticket.ThreadKey,
		"session_id":        tk.Ticket.SessionID,
		"created_at":        tk.Ticket.CreatedAt,
		"latest_assignment": assignment,
		"waits_for_person":  tk.WaitsForPerson,
		"last_event":        tk.LastEvent,
		"last_event_at":     tk.LastEventAt,
	}
}

// actorOf reads the acting person from the query string, defaulting to the
// admin identity. P0's admin port carries no authentication (it is mounted
// inside the cluster boundary like the push receiver); the actor is
// attribution, not authorization.
func actorOf(r *http.Request) string {
	if actor := r.URL.Query().Get("actor"); actor != "" {
		return actor
	}
	return "admin"
}

func writeAdminError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"reason": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
