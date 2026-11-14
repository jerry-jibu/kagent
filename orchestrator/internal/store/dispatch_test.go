package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// dispatchFixture creates one accepted ticket (with its pending assignment)
// and returns the store plus the ticket id.
func dispatchFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := openTestStore(t)
	mustMigrate(t, s)
	created, err := s.CreateTicket(context.Background(), acceptInput("github:acme/api", "issue-comment:7001"))
	if err != nil {
		t.Fatalf("CreateTicket failed: %v", err)
	}
	return s, created.Ticket.ID
}

func TestMarkAssignmentDispatched(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a pending assignment on a pending ticket, when the dispatch lands, assert that the assignment carries the task id in running state and the ticket is running with the session context in one transaction", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, err := s.Assignments(ctx, ticketID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}

		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-77", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}

		got, err := s.Assignments(ctx, ticketID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if got[0].Status != store.AssignmentStatusRunning || got[0].TaskID == nil || *got[0].TaskID != "task-77" {
			t.Fatalf("assignment = %+v, want running with task-77", got[0])
		}
		tk, err := s.Ticket(ctx, ticketID)
		if err != nil {
			t.Fatalf("Ticket failed: %v", err)
		}
		if tk.Status != store.TicketStatusRunning {
			t.Fatalf("ticket status = %q, want running", tk.Status)
		}
		if tk.SessionID != "ctx-77" {
			t.Fatalf("session context = %q, want ctx-77 (session continuity)", tk.SessionID)
		}
	})

	t.Run("Given a ticket already bound to a session context, when a continuation dispatch lands with the same context, assert that the context does not regress", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-1", "ctx-1", time.Time{}); err != nil {
			t.Fatalf("first dispatch failed: %v", err)
		}
		followup, err := s.CreateFollowupAssignment(ctx, ticketID)
		if err != nil {
			t.Fatalf("CreateFollowupAssignment failed: %v", err)
		}
		if err := s.MarkAssignmentDispatched(ctx, followup.ID, ticketID, "task-2", "ctx-1", time.Time{}); err != nil {
			t.Fatalf("continuation dispatch failed: %v", err)
		}
		tk, _ := s.Ticket(ctx, ticketID)
		if tk.SessionID != "ctx-1" {
			t.Fatalf("session context = %q, want ctx-1 unchanged", tk.SessionID)
		}
	})

	t.Run("Given an assignment no longer pending, when the dispatch is recorded, assert that the store refuses the double transition", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-1", "ctx-1", time.Time{}); err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-1", "ctx-1", time.Time{}); !errors.Is(err, store.ErrAssignmentNotPending) {
			t.Fatalf("err = %v, want ErrAssignmentNotPending", err)
		}
	})
}

func TestRecordDispatchFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a dispatch attempt that failed, when the failure is recorded, assert that the attempt counter and error land with an audit row and the assignment stays pending", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)

		if err := s.RecordDispatchFailure(ctx, assignments[0].ID, "gateway timed out"); err != nil {
			t.Fatalf("RecordDispatchFailure failed: %v", err)
		}

		pending, err := s.PendingDispatches(ctx)
		if err != nil {
			t.Fatalf("PendingDispatches failed: %v", err)
		}
		if len(pending) != 1 || pending[0].Assignment.ID != assignments[0].ID {
			t.Fatalf("pending = %+v, want the still-pending assignment", pending)
		}
		if pending[0].Assignment.DispatchAttempts != 1 || pending[0].Assignment.LastError != "gateway timed out" {
			t.Fatalf("attempts/error = %d/%q, want 1/gateway timed out",
				pending[0].Assignment.DispatchAttempts, pending[0].Assignment.LastError)
		}
		events, err := s.AuditEvents(ctx, ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) == 0 || events[len(events)-1].Event != "dispatch.failed" {
			t.Fatalf("audit trail = %+v, want a dispatch.failed row", events)
		}
	})
}

func TestApplyAssignmentTerminal(t *testing.T) {
	ctx := context.Background()

	// dispatchedFixture returns a store whose single assignment is running
	// with a task id and the ticket running.
	dispatchedFixture := func(t *testing.T) (*store.Store, string, string) {
		t.Helper()
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-77", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}
		return s, ticketID, assignments[0].ID
	}

	t.Run("Given a running assignment and a terminal report, when the terminal outcome is applied, assert that assignment and ticket move in one transaction with the audit row", func(t *testing.T) {
		s, ticketID, assignmentID := dispatchedFixture(t)

		outcome, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignmentID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusEnded,
			Event:            "report.ready",
			Detail:           map[string]any{"summary": "PR ready"},
		})
		if err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}
		if !outcome.Applied || !outcome.TicketChanged {
			t.Fatalf("outcome = %+v, want both transitions applied", outcome)
		}

		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusEnded || assignments[0].Report != "ready" {
			t.Fatalf("assignment = %+v, want ended with the report recorded", assignments[0])
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusEnded {
			t.Fatalf("ticket status = %q, want ended", status)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		if len(events) == 0 || events[len(events)-1].Event != "report.ready" {
			t.Fatalf("audit trail = %+v, want the report row", events)
		}
	})

	t.Run("Given the same terminal outcome replayed, when it is applied again, assert that the replay is a no-op that reports not applied (push then reconcile double delivery)", func(t *testing.T) {
		s, ticketID, assignmentID := dispatchedFixture(t)
		plan := store.TerminalPlan{
			AssignmentID:     assignmentID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusEnded,
			Event:            "report.ready",
			Detail:           map[string]any{},
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, plan); err != nil {
			t.Fatalf("first apply failed: %v", err)
		}

		outcome, err := s.ApplyAssignmentTerminal(ctx, plan)
		if err != nil {
			t.Fatalf("replay failed: %v", err)
		}
		if outcome.Applied {
			t.Fatalf("replay outcome = %+v, want applied=false", outcome)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		count := 0
		for _, ev := range events {
			if ev.Event == "report.ready" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("report.ready audit rows = %d, want exactly 1 (幂等)", count)
		}
	})

	t.Run("Given an unresolved outcome (task missing at the gateway), when it is applied, assert that the assignment is marked unresolved and the ticket needs attention — never replayed", func(t *testing.T) {
		s, ticketID, assignmentID := dispatchedFixture(t)

		outcome, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignmentID,
			AssignmentStatus: store.AssignmentStatusUnresolved,
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusNeedsAttention,
			Event:            "assignment.unresolved",
			Detail:           map[string]any{"reason": "task not found"},
		})
		if err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}
		if !outcome.Applied {
			t.Fatalf("outcome = %+v, want applied", outcome)
		}
		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusUnresolved {
			t.Fatalf("assignment status = %q, want unresolved (红线标记)", assignments[0].Status)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket status = %q, want needs-attention", status)
		}
	})

	t.Run("Given a ticket already terminal, when a late terminal report arrives for its running assignment, assert that the assignment still settles while the ticket does not resurrect", func(t *testing.T) {
		s, ticketID, assignmentID := dispatchedFixture(t)
		// End the ticket first (e.g. a follow-up assignment's report won):
		// the original settles under progress, the follow-up lands ready.
		other, err := s.CreateFollowupAssignment(ctx, ticketID)
		if err != nil {
			t.Fatalf("CreateFollowupAssignment failed: %v", err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignmentID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "progress",
			TicketID:         ticketID,
		}); err != nil {
			t.Fatalf("progress apply failed: %v", err)
		}
		if err := s.MarkAssignmentDispatched(ctx, other.ID, ticketID, "task-78", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("followup dispatch failed: %v", err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     other.ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusEnded,
			Event:            "report.ready",
			Detail:           map[string]any{},
		}); err != nil {
			t.Fatalf("ready apply failed: %v", err)
		}

		late, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignmentID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusEnded,
			Event:            "report.ready",
			Detail:           map[string]any{},
		})
		if err != nil {
			t.Fatalf("late apply failed: %v", err)
		}
		if late.Applied || late.TicketChanged {
			t.Fatalf("late outcome = %+v, want a full no-op against an ended ticket", late)
		}
	})
}

func TestMarkTicketNeedsAttention(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a running ticket whose task paused for input, when the detection point fires, assert that the ticket moves to needs-attention with an audit row while the assignment keeps running", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-77", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		changed, err := s.MarkTicketNeedsAttention(ctx, ticketID, "task paused in TASK_STATE_INPUT_REQUIRED")
		if err != nil {
			t.Fatalf("MarkTicketNeedsAttention failed: %v", err)
		}
		if !changed {
			t.Fatal("the first detection must change the ticket")
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket status = %q, want needs-attention", status)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		if len(events) == 0 || events[len(events)-1].Event != "ticket.needs_attention" {
			t.Fatalf("audit trail = %+v, want the detection row", events)
		}

		// Idempotent: repeated detections change nothing.
		changed, err = s.MarkTicketNeedsAttention(ctx, ticketID, "again")
		if err != nil || changed {
			t.Fatalf("repeat detection = (%v, %v), want (false, nil)", changed, err)
		}
	})
}

func TestRunningAssignmentCandidates(t *testing.T) {
	ctx := context.Background()

	t.Run("Given dispatched and undispatched tickets, when the reconcile scan runs, assert that only running assignments bound to live tickets are candidates", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-77", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}

		// A second ticket that was never dispatched must not reconcile.
		if _, err := s.CreateTicket(ctx, acceptInput("github:acme/api", "issue-comment:7002")); err != nil {
			t.Fatalf("second CreateTicket failed: %v", err)
		}

		candidates, err := s.RunningAssignmentCandidates(ctx)
		if err != nil {
			t.Fatalf("RunningAssignmentCandidates failed: %v", err)
		}
		if len(candidates) != 1 || candidates[0].Assignment.ID != assignments[0].ID || candidates[0].Ticket.ID != ticketID {
			t.Fatalf("candidates = %+v, want exactly the dispatched ticket's assignment", candidates)
		}
		if candidates[0].Assignment.TaskID == nil || *candidates[0].Assignment.TaskID != "task-77" {
			t.Fatalf("candidate task id = %+v, want task-77", candidates[0].Assignment.TaskID)
		}
	})
}

func TestFollowupCandidates(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a running ticket whose only assignment ended with a progress report, when the follow-up scan runs, assert that the ticket is a candidate with its follow-up count", func(t *testing.T) {
		s, ticketID := dispatchFixture(t)
		assignments, _ := s.Assignments(ctx, ticketID)
		if err := s.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-77", "ctx-77", time.Time{}); err != nil {
			t.Fatalf("dispatch failed: %v", err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignments[0].ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "progress",
			TicketID:         ticketID,
		}); err != nil {
			t.Fatalf("progress apply failed: %v", err)
		}

		candidates, err := s.FollowupCandidates(ctx)
		if err != nil {
			t.Fatalf("FollowupCandidates failed: %v", err)
		}
		if len(candidates) != 1 || candidates[0].TicketID != ticketID || candidates[0].Followups != 0 {
			t.Fatalf("candidates = %+v, want the progress ticket with 0 follow-ups", candidates)
		}
		if candidates[0].LastEndedAt.IsZero() {
			t.Fatal("candidate must carry the last ended assignment time (the follow-up delay anchor)")
		}

		t.Run("Given the follow-up cap already spent, when the scan runs, assert that the ticket is no longer a candidate", func(t *testing.T) {
			for range 3 {
				a, err := s.CreateFollowupAssignment(ctx, ticketID)
				if err != nil {
					t.Fatalf("CreateFollowupAssignment failed: %v", err)
				}
				if err := s.MarkAssignmentDispatched(ctx, a.ID, ticketID, "task-"+a.ID, "ctx-77", time.Time{}); err != nil {
					t.Fatalf("dispatch failed: %v", err)
				}
				if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
					AssignmentID:     a.ID,
					AssignmentStatus: store.AssignmentStatusEnded,
					ReportState:      "progress",
					TicketID:         ticketID,
				}); err != nil {
					t.Fatalf("progress apply failed: %v", err)
				}
			}
			candidates, err := s.FollowupCandidates(ctx)
			if err != nil {
				t.Fatalf("FollowupCandidates failed: %v", err)
			}
			if len(candidates) != 1 || candidates[0].Followups != 3 {
				t.Fatalf("candidates = %+v, want the ticket reporting 3 spent follow-ups", candidates)
			}
		})
	})
}

func TestTriggerColumns(t *testing.T) {
	ctx := context.Background()

	t.Run("Given an accepted ticket, when its row is read, assert that the originating trigger identity is recorded for the dispatch document", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		created, err := s.CreateTicket(ctx, acceptInput("github:acme/api", "issue-comment:7003"))
		if err != nil {
			t.Fatalf("CreateTicket failed: %v", err)
		}
		tk, err := s.Ticket(ctx, created.Ticket.ID)
		if err != nil {
			t.Fatalf("Ticket failed: %v", err)
		}
		if tk.TriggerSource != "github:acme/api" || tk.TriggerDeterministicID != "issue-comment:7003" {
			t.Fatalf("trigger identity = %s/%s, want the creating trigger", tk.TriggerSource, tk.TriggerDeterministicID)
		}
	})
}
