package server_test

// The minimal admin surface over HTTP (T8, issue #9 AC: 管理入口可完成
// list/retry/stop 三个动作且全部留审计). The store transactions and their
// audit rows are store-level tested; here the transport maps wire to domain
// and status codes — with a scripted AdminService so a broken endpoint can
// never hide behind a passing store.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/server"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// fakeAdmin scripts the three admin operations and records the actor each
// call carried.
type fakeAdmin struct {
	tickets []store.AttentionTicket
	err     error

	retryID    string
	retryActor string
	retryErr   error
	retryOut   store.Assignment

	stopID    string
	stopActor string
	stopErr   error
}

func (f *fakeAdmin) NeedsAttentionTickets(ctx context.Context) ([]store.AttentionTicket, error) {
	return f.tickets, f.err
}

func (f *fakeAdmin) RetryTicket(ctx context.Context, ticketID, actor string) (store.Assignment, error) {
	f.retryID, f.retryActor = ticketID, actor
	if f.retryErr != nil {
		return store.Assignment{}, f.retryErr
	}
	return f.retryOut, nil
}

func (f *fakeAdmin) StopTicket(ctx context.Context, ticketID, actor string) error {
	f.stopID, f.stopActor = ticketID, actor
	return f.stopErr
}

func newAdminHandler(admin server.AdminService) http.Handler {
	return server.NewHandler(
		func() error { return nil },
		func() (string, time.Time, bool) { return "abc123", time.Now(), true },
		nil,
		admin,
	)
}

func TestAdminNeedsAttentionList(t *testing.T) {
	t.Run("Given one held ticket, when a caller GETs /admin/needs-attention, assert that the row carries the ticket, its latest assignment and the waitsForPerson flag (AC: list)", func(t *testing.T) {
		admin := &fakeAdmin{tickets: []store.AttentionTicket{{
			Ticket: store.Ticket{
				ID:           "ticket-1",
				ThreadSource: "github:acme/api",
				ThreadKey:    "issue:42",
				Status:       store.TicketStatusNeedsAttention,
			},
			LatestAssignment: store.Assignment{ID: "assign-1", Status: "unresolved"},
			WaitsForPerson:   true,
			LastEvent:        "ticket.needs_attention",
			LastEventAt:      time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC),
		}}}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/needs-attention", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Tickets []struct {
				ID             string `json:"id"`
				Status         string `json:"status"`
				ThreadSource   string `json:"thread_source"`
				WaitsForPerson bool   `json:"waits_for_person"`
				LastEvent      string `json:"last_event"`
				Assignment     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"latest_assignment"`
			} `json:"tickets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response must be JSON: %v (body: %s)", err, rec.Body.String())
		}
		if len(body.Tickets) != 1 {
			t.Fatalf("tickets = %+v, want the single held row", body.Tickets)
		}
		row := body.Tickets[0]
		if row.ID != "ticket-1" || row.Status != "needs-attention" || !row.WaitsForPerson || row.LastEvent != "ticket.needs_attention" {
			t.Fatalf("row = %+v, want the held ticket's context", row)
		}
		if row.Assignment.ID != "assign-1" || row.Assignment.Status != "unresolved" {
			t.Fatalf("latest assignment = %+v, want the unresolved assignment", row.Assignment)
		}
	})

	t.Run("Given a store failure, when a caller GETs /admin/needs-attention, assert that it answers 503 without leaking internals", func(t *testing.T) {
		admin := &fakeAdmin{err: errors.New("db down")}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/needs-attention", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d", rec.Code)
		}
	})

	t.Run("Given no admin service mounted, when a caller GETs /admin/needs-attention, assert that it answers 404 (端点未挂载)", func(t *testing.T) {
		h := server.NewHandler(
			func() error { return nil },
			func() (string, time.Time, bool) { return "abc123", time.Now(), true },
			nil,
			nil,
		)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/needs-attention", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", rec.Code)
		}
	})
}

func TestAdminRetry(t *testing.T) {
	t.Run("Given a held ticket, when the admin POSTs retry with an actor, assert that the service receives the ticket id and actor and the answer carries the minted assignment (AC: retry)", func(t *testing.T) {
		admin := &fakeAdmin{retryOut: store.Assignment{ID: "assign-9", Status: "pending", Kind: "followup"}}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/retry?actor=jerry", nil)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if admin.retryID != "ticket-1" || admin.retryActor != "jerry" {
			t.Fatalf("service saw ticket=%q actor=%q, want ticket-1/jerry", admin.retryID, admin.retryActor)
		}
		var body struct {
			AssignmentID string `json:"assignment_id"`
			Status       string `json:"ticket_status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response must be JSON: %v", err)
		}
		if body.AssignmentID != "assign-9" || body.Status != store.TicketStatusRunning {
			t.Fatalf("body = %+v, want the minted assignment and the running ticket", body)
		}
	})

	t.Run("Given a default actor, when the admin POSTs retry without an actor, assert that the service sees the admin identity", func(t *testing.T) {
		admin := &fakeAdmin{retryOut: store.Assignment{ID: "assign-9"}}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/retry", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		if admin.retryActor != "admin" {
			t.Fatalf("actor = %q, want the admin default", admin.retryActor)
		}
	})

	t.Run("Given a ticket that is not held, when the admin POSTs retry, assert that it answers 409 (retry 只针对挂起工单)", func(t *testing.T) {
		admin := &fakeAdmin{retryErr: store.ErrTicketNotHeld}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/retry", nil))

		if rec.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d", rec.Code)
		}
	})

	t.Run("Given an unknown ticket, when the admin POSTs retry, assert that it answers 404", func(t *testing.T) {
		admin := &fakeAdmin{retryErr: store.ErrTicketNotFound}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/none/retry", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", rec.Code)
		}
	})
}

func TestAdminStop(t *testing.T) {
	t.Run("Given a live ticket, when the admin POSTs stop, assert that the service receives the stop and the answer reports the failed ticket (AC: stop)", func(t *testing.T) {
		admin := &fakeAdmin{}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/stop?actor=jerry", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if admin.stopID != "ticket-1" || admin.stopActor != "jerry" {
			t.Fatalf("service saw ticket=%q actor=%q, want ticket-1/jerry", admin.stopID, admin.stopActor)
		}
		var body struct {
			Status string `json:"ticket_status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response must be JSON: %v", err)
		}
		if body.Status != store.TicketStatusFailed {
			t.Fatalf("ticket_status = %q, want failed", body.Status)
		}
	})

	t.Run("Given a terminal ticket, when the admin POSTs stop, assert that it answers 409", func(t *testing.T) {
		admin := &fakeAdmin{stopErr: store.ErrTicketTerminal}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/stop", nil))

		if rec.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d", rec.Code)
		}
	})

	t.Run("Given a store failure, when the admin POSTs stop, assert that it answers 500", func(t *testing.T) {
		admin := &fakeAdmin{stopErr: errors.New("db down")}
		h := newAdminHandler(admin)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/tickets/ticket-1/stop", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("want 500, got %d", rec.Code)
		}
	})
}
