// Package server exposes the orchestrator's transport endpoints: healthz
// for k8s probes, the A2A push receiver for task terminal states
// (docs/designs 0001 D2), the minimal admin surface for the needs-attention
// loop (T8: list / retry / stop), and admin endpoints for verifying runtime
// state.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
// adminToken guards the MUTATING admin endpoints (retry/stop) when set —
// Authorization: Bearer <token> required (PR #11 review, Required #12 /
// design-R3-2: a forged in-cluster retry/stop drives the ticket machine).
// Empty keeps the P0 dev behavior. The admin GET endpoints (healthz,
// /admin/policy, /admin/needs-attention) are deliberately UNAUTHENTICATED in
// P0: the port is reachable only inside the cluster boundary (ClusterIP,
// no Ingress). That is a P0 scope decision, not an oversight — an operator
// exposing this port beyond the cluster (Ingress, LoadBalancer, port-forward
// to untrusted networks) MUST front it with authenticating proxy or enable
// the admin token and treat reads as public.
func NewHandler(ready Readiness, policy PolicyView, push http.Handler, admin AdminService, adminToken string) http.Handler {
	mux := http.NewServeMux()
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		if adminToken == "" {
			return next
		}
		return func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(adminToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized,
					map[string]string{"reason": "admin mutation requires the admin token (Authorization: Bearer)"})
				return
			}
			next(w, r)
		}
	}
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
				// The cause rides the response body, so a broken store scan
				// never answers a bare 503. (The Pass/eval/delivery loops log
				// the same store failure on every tick; the admin surface is
				// a human's view, so the reason must be visible where the
				// human is looking.)
				writeJSON(w, http.StatusServiceUnavailable,
					map[string]string{"reason": fmt.Sprintf("failed to list needs-attention tickets: %v", err)})
				return
			}
			rows := make([]map[string]any, 0, len(tickets))
			for _, tk := range tickets {
				rows = append(rows, attentionRow(tk))
			}
			writeJSON(w, http.StatusOK, map[string]any{"tickets": rows})
		})
		mux.HandleFunc("POST /admin/tickets/{id}/retry", guard(func(w http.ResponseWriter, r *http.Request) {
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
		}))
		mux.HandleFunc("POST /admin/tickets/{id}/stop", guard(func(w http.ResponseWriter, r *http.Request) {
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
		}))
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
