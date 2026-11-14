package dispatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// receiverFixture returns a handler over a store whose fixture assignment is
// running with task-9 bound.
func receiverFixture(t *testing.T) (*store.Store, string, http.Handler) {
	t.Helper()
	s, ticketID, _, _ := dispatchedFixture(t)
	return s, ticketID, dispatch.NewReceiver(dispatch.New(s, a2a.NewFake(), dispatch.Options{WorkerAgent: "kagent/worker"}))
}

func postPush(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, dispatch.PushReceiverPath, strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func TestPushReceiver(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a push event wrapping a terminal task, when it arrives, assert that the endpoint accepts it and the report drives the ticket transition (AC: push 终态到达)", func(t *testing.T) {
		s, ticketID, h := receiverFixture(t)

		rec := postPush(t, h, `{"task":{"id":"task-9","contextId":"ctx-9","status":{"state":"TASK_STATE_COMPLETED","message":{"role":"ROLE_AGENT","parts":[{"text":"{\"kind\":\"report\",\"state\":\"ready\"}"}],"messageId":"m1"}}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}

		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want ended via the pushed report", status)
		}
	})

	t.Run("Given a push event wrapping an input-required status update, when it arrives, assert that the endpoint registers the pause as needs-attention (0002 §3)", func(t *testing.T) {
		s, ticketID, h := receiverFixture(t)

		rec := postPush(t, h, `{"statusUpdate":{"taskId":"task-9","contextId":"ctx-9","status":{"state":"TASK_STATE_INPUT_REQUIRED","message":{"role":"ROLE_AGENT","parts":[{"text":"which approach?"}],"messageId":"m2"}}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}

		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusRunning {
			t.Fatalf("assignment = %q, want still running", assignments[0].Status)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want needs-attention", status)
		}
	})

	t.Run("Given a push event for a task the orchestrator never dispatched, when it arrives, assert that the endpoint answers 404 so the mismatch is loud", func(t *testing.T) {
		_, _, h := receiverFixture(t)

		rec := postPush(t, h, `{"task":{"id":"task-foreign","status":{"state":"TASK_STATE_COMPLETED"}}}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("Given a body that is not an event wrapper, when it arrives, assert that the endpoint answers 400 (malformed durable data is never silently omitted)", func(t *testing.T) {
		_, _, h := receiverFixture(t)

		for _, body := range []string{"", "not json", `{"neither":"task nor update"}`} {
			rec := postPush(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("body %q: status = %d, want 400", body, rec.Code)
			}
		}
	})

	t.Run("Given a replayed push for an already settled task, when it arrives, assert that the endpoint still answers 200 (gateway retries must not error-loop)", func(t *testing.T) {
		_, _, h := receiverFixture(t)

		body := `{"task":{"id":"task-9","status":{"state":"TASK_STATE_COMPLETED","message":{"role":"ROLE_AGENT","parts":[{"text":"{\"kind\":\"report\",\"state\":\"ready\"}"}],"messageId":"m1"}}}}`
		if rec := postPush(t, h, body); rec.Code != http.StatusOK {
			t.Fatalf("first push status = %d, want 200", rec.Code)
		}
		if rec := postPush(t, h, body); rec.Code != http.StatusOK {
			t.Fatalf("replayed push status = %d, want 200 (幂等)", rec.Code)
		}
	})
}
