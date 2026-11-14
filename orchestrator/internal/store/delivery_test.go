package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// deliveryFixture creates one accepted ticket, dispatches its assignment as
// task-9001, and returns the store, the ticket id and the running assignment.
func deliveryFixture(t *testing.T) (*store.Store, string, store.Assignment) {
	t.Helper()
	s, ticketID := dispatchFixture(t)
	ctx := context.Background()
	if err := s.MarkAssignmentDispatched(ctx, store.AssignmentIDForTrigger("github:acme/api", "issue-comment:7001"),
		ticketID, "task-9001", "ctx-9001", time.Time{}); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}
	assignments, err := s.Assignments(ctx, ticketID)
	if err != nil {
		t.Fatalf("Assignments failed: %v", err)
	}
	return s, ticketID, assignments[0]
}

// readyPlan is the terminal plan a ready report produces under Alternative E:
// the assignment ends, the ticket ends, and the result becomes deliverable.
func readyPlan(a store.Assignment, ticketID string) store.TerminalPlan {
	return store.TerminalPlan{
		AssignmentID:     a.ID,
		AssignmentStatus: store.AssignmentStatusEnded,
		ReportState:      "ready",
		TicketID:         ticketID,
		TicketStatus:     store.TicketStatusEnded,
		DeliveryPending:  true,
		Event:            "report.ready",
		Detail: map[string]any{
			"state":     "ready",
			"task_id":   "task-9001",
			"summary":   "rewrote the parser",
			"artifacts": []string{"internal/parser/parser.go", "patches/notes.md"},
		},
	}
}

func TestDeliveryState(t *testing.T) {
	t.Run("Given a dispatched task that settles under a ready report, when the terminal plan applies, assert that the ticket ends with its result pending delivery and the scan lists it as a deliverable carrying the baseline", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()

		if err := s.RecordTicketBaseline(ctx, ticketID, "main", "0123456789abcdef0123456789abcdef01234567"); err != nil {
			t.Fatalf("RecordTicketBaseline failed: %v", err)
		}

		outcome, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID))
		if err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}
		if !outcome.Applied || !outcome.TicketChanged {
			t.Fatalf("ready transition must apply, got %+v", outcome)
		}

		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("candidates = %d, want 1: %+v", len(candidates), candidates)
		}
		c := candidates[0]
		if c.Ticket.ID != ticketID {
			t.Fatalf("candidate ticket = %s, want %s", c.Ticket.ID, ticketID)
		}
		if c.Ticket.Status != store.TicketStatusEnded {
			t.Fatalf("candidate ticket status = %s, want ended", c.Ticket.Status)
		}
		if c.BaseBranch != "main" || c.BaseCommit != "0123456789abcdef0123456789abcdef01234567" {
			t.Fatalf("candidate baseline = %s@%s, want main@0123...", c.BaseBranch, c.BaseCommit)
		}
		if c.Ticket.Mandate.Scope.Repo != "acme/api" || c.Ticket.Mandate.Scope.Issue != 42 {
			t.Fatalf("candidate mandate scope = %+v, want acme/api#42", c.Ticket.Mandate.Scope)
		}
	})

	t.Run("Given a ready plan replayed after it already settled, when it applies again, assert that the settled guard absorbs it and the result is not scanned twice", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("first apply failed: %v", err)
		}

		outcome, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID))
		if err != nil {
			t.Fatalf("replay apply failed: %v", err)
		}
		if outcome.Applied || outcome.TicketChanged {
			t.Fatalf("replay must be a no-op, got %+v", outcome)
		}

		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("candidates = %d, want 1 (delivery state not re-set by the replay)", len(candidates))
		}
	})

	t.Run("Given a terminal plan for a non-ready report, when it applies, assert that no delivery becomes pending", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()

		plan := readyPlan(a, ticketID)
		plan.ReportState = "failed"
		plan.TicketStatus = store.TicketStatusFailed
		plan.DeliveryPending = false
		plan.Event = "report.failed"
		if _, err := s.ApplyAssignmentTerminal(ctx, plan); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 0 {
			t.Fatalf("candidates = %d, want 0 for a failed report", len(candidates))
		}
	})

	t.Run("Given a pending delivery, when the result is delivered with its comment effect, assert that the state, the queued effect and the audit row commit together and a replay neither re-enqueues nor re-audits", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		effect := store.OutboxEffect{
			Channel:        "github",
			Address:        "acme/api#42",
			IdempotencyKey: "result:" + ticketID,
			Payload:        []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"body":"result"}`),
		}
		applied, err := s.MarkTicketDelivered(ctx, store.DeliveredPlan{
			TicketID: ticketID,
			PRNumber: 7,
			PRURL:    "https://github.com/acme/api/pull/7",
			Branch:   ticketID,
			Commit:   "0123456789abcdef0123456789abcdef01234567",
			Effect:   effect,
			Detail:   map[string]any{"pr_number": 7},
		})
		if err != nil {
			t.Fatalf("MarkTicketDelivered failed: %v", err)
		}
		if !applied {
			t.Fatalf("first delivery must apply")
		}

		applied, err = s.MarkTicketDelivered(ctx, store.DeliveredPlan{
			TicketID: ticketID,
			PRNumber: 7,
			PRURL:    "https://github.com/acme/api/pull/7",
			Branch:   ticketID,
			Commit:   "0123456789abcdef0123456789abcdef01234567",
			Effect:   effect,
			Detail:   map[string]any{"pr_number": 7},
		})
		if err != nil {
			t.Fatalf("replay MarkTicketDelivered failed: %v", err)
		}
		if applied {
			t.Fatalf("replay must report applied=false")
		}

		effects, err := s.Effects(ctx)
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		results := 0
		for _, e := range effects {
			if e.IdempotencyKey == "result:"+ticketID {
				results++
				if e.State != store.EffectPending {
					t.Fatalf("result effect state = %s, want pending", e.State)
				}
			}
		}
		if results != 1 {
			t.Fatalf("result effects = %d, want 1 (replay must not re-enqueue)", results)
		}

		events, err := s.AuditEvents(ctx, ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		deliveries := 0
		for _, ev := range events {
			if ev.Event == "delivery.delivered" {
				deliveries++
				if ev.Detail["pr_url"] != "https://github.com/acme/api/pull/7" {
					t.Fatalf("delivery audit pr_url = %v", ev.Detail["pr_url"])
				}
			}
		}
		if deliveries != 1 {
			t.Fatalf("delivery.delivered audits = %d, want 1", deliveries)
		}

		status, err := s.TicketStatus(ctx, ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusEnded {
			t.Fatalf("delivered ticket status = %s, want ended (delivery does not change the ticket state)", status)
		}
	})

	t.Run("Given a pending delivery whose patch is missing, when the delivery fails permanently, assert that the ticket transitions to failed with its audit and a replay is a no-op", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		applied, err := s.FailTicketDelivery(ctx, ticketID, "no artifacts in ready report")
		if err != nil {
			t.Fatalf("FailTicketDelivery failed: %v", err)
		}
		if !applied {
			t.Fatalf("first delivery failure must apply")
		}

		applied, err = s.FailTicketDelivery(ctx, ticketID, "no artifacts in ready report")
		if err != nil {
			t.Fatalf("replay FailTicketDelivery failed: %v", err)
		}
		if applied {
			t.Fatalf("replay must report applied=false")
		}

		status, err := s.TicketStatus(ctx, ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusFailed {
			t.Fatalf("ticket status = %s, want failed", status)
		}

		events, err := s.AuditEvents(ctx, ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		failures := 0
		for _, ev := range events {
			if ev.Event == "delivery.failed" {
				failures++
				if ev.Detail["reason"] != "no artifacts in ready report" {
					t.Fatalf("delivery.failed reason = %v", ev.Detail["reason"])
				}
			}
		}
		if failures != 1 {
			t.Fatalf("delivery.failed audits = %d, want 1 (replay must not re-audit)", failures)
		}

		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 0 {
			t.Fatalf("failed delivery must leave the scan, got %d candidates", len(candidates))
		}
	})

	t.Run("Given a delivered ticket in a mixed scan, when PendingDeliveries lists candidates, assert that only pending deliveries surface in enqueue order", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()

		// Second ticket: created, dispatched, ready — pending delivery.
		created2, err := s.CreateTicket(ctx, acceptInput("github:acme/api", "issue-comment:7102"))
		if err != nil {
			t.Fatalf("CreateTicket(2) failed: %v", err)
		}
		assignments2, err := s.Assignments(ctx, created2.Ticket.ID)
		if err != nil {
			t.Fatalf("Assignments(2) failed: %v", err)
		}
		if err := s.MarkAssignmentDispatched(ctx, assignments2[0].ID, created2.Ticket.ID, "task-9002", "ctx-9002", time.Time{}); err != nil {
			t.Fatalf("MarkAssignmentDispatched(2) failed: %v", err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(assignments2[0], created2.Ticket.ID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal(2) failed: %v", err)
		}

		// First ticket: ready then delivered.
		if err := s.RecordTicketBaseline(ctx, ticketID, "main", "0123456789abcdef0123456789abcdef01234567"); err != nil {
			t.Fatalf("RecordTicketBaseline failed: %v", err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}
		if _, err := s.MarkTicketDelivered(ctx, store.DeliveredPlan{
			TicketID: ticketID,
			PRNumber: 7,
			PRURL:    "https://github.com/acme/api/pull/7",
			Branch:   ticketID,
			Commit:   "0123456789abcdef0123456789abcdef01234567",
			Effect: store.OutboxEffect{
				Channel:        "github",
				Address:        "acme/api#42",
				IdempotencyKey: "result:" + ticketID,
				Payload:        []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"body":"result"}`),
			},
		}); err != nil {
			t.Fatalf("MarkTicketDelivered failed: %v", err)
		}

		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("candidates = %d, want 1 (only the undelivered ticket)", len(candidates))
		}
		if candidates[0].Ticket.ID != created2.Ticket.ID {
			t.Fatalf("candidate = %s, want %s", candidates[0].Ticket.ID, created2.Ticket.ID)
		}
	})

	t.Run("Given a ticket whose result effect payload is not JSON, when MarkTicketDelivered is called, assert that the store refuses the whole transition", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		_, err := s.MarkTicketDelivered(ctx, store.DeliveredPlan{
			TicketID: ticketID,
			PRNumber: 7,
			PRURL:    "https://github.com/acme/api/pull/7",
			Effect: store.OutboxEffect{
				Channel:        "github",
				Address:        "acme/api#42",
				IdempotencyKey: "result:" + ticketID,
				Payload:        []byte(`not-json`),
			},
		})
		if err == nil {
			t.Fatalf("MarkTicketDelivered must refuse a non-JSON effect payload")
		}
		candidates, err := s.PendingDeliveries(ctx)
		if err != nil {
			t.Fatalf("PendingDeliveries failed: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("refused delivery must stay pending, got %d candidates", len(candidates))
		}
	})
}

// TestDeliveryAuditDetailShape guards the audit contract the report comment
// reads: the ready plan's detail carries the artifacts and summary verbatim.
func TestDeliveryAuditDetailShape(t *testing.T) {
	t.Run("Given a ready plan, when it applies, assert that the audit detail keeps summary and artifacts for the deliverer to read back", func(t *testing.T) {
		s, ticketID, a := deliveryFixture(t)
		ctx := context.Background()
		if _, err := s.ApplyAssignmentTerminal(ctx, readyPlan(a, ticketID)); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		events, err := s.AuditEvents(ctx, ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		var ready map[string]any
		for _, ev := range events {
			if ev.Event == "report.ready" {
				ready = ev.Detail
			}
		}
		if ready == nil {
			t.Fatalf("no report.ready audit row")
		}
		if ready["summary"] != "rewrote the parser" {
			t.Fatalf("audit summary = %v", ready["summary"])
		}
		artifacts, ok := ready["artifacts"].([]any)
		if !ok || len(artifacts) != 2 {
			t.Fatalf("audit artifacts = %#v, want two entries", ready["artifacts"])
		}
		if artifacts[0] != "internal/parser/parser.go" {
			t.Fatalf("artifacts[0] = %v", artifacts[0])
		}
	})
}

