package store_test

// The minimal admin surface (T8, issue #9): list needs-attention, retry, stop
// — every mutating action audited, every transition guarded so an admin
// action can never create a state the Engine could not have written. The
// followup-input record is the same transactional shape: a person's message
// on an owned thread is recorded as the owner's input and a terminal decision
// together.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// adminTicketFixture creates an accepted ticket with its initial assignment
// and walks it to the requested state through the public transitions.
func adminTicketFixture(t *testing.T, s *store.Store, source, deterministicID, threadKey string) (store.Ticket, store.Assignment) {
	t.Helper()
	ctx := context.Background()
	created, err := s.CreateTicket(ctx, store.TicketInput{
		Source:          source,
		DeterministicID: deterministicID,
		ThreadKey:       threadKey,
		Decision: store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "admin surface test",
		},
		Mandate: store.Mandate{
			PolicyVersion: "abc123",
			Strength:      "explicit",
			Sender:        "alice",
			Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
		},
	})
	if err != nil {
		t.Fatalf("CreateTicket failed: %v", err)
	}
	assignments, err := s.Assignments(ctx, created.Ticket.ID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("Assignments = %+v, %v; want the initial assignment", assignments, err)
	}
	return created.Ticket, assignments[0]
}

// dispatchTask binds the assignment's task (the dispatcher's mark).
func dispatchTask(t *testing.T, s *store.Store, a store.Assignment, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.MarkAssignmentDispatched(ctx, a.ID, a.TicketID, taskID, "ctx-"+taskID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}
}

func TestNeedsAttentionTickets(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a held ticket and a running one, when the admin list runs, assert that only the held ticket is listed with its latest assignment and last event (AC: 列出 needs-attention)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		held, heldA := adminTicketFixture(t, s, "github:acme/api", "issue-comment:20", "issue:42")
		dispatchTask(t, s, heldA, "task-20")
		if _, err := s.MarkTicketNeedsAttention(ctx, held.ID, "task paused for a person"); err != nil {
			t.Fatalf("MarkTicketNeedsAttention failed: %v", err)
		}
		live, _ := adminTicketFixture(t, s, "github:acme/api", "issue-comment:21", "issue:43")

		got, err := s.NeedsAttentionTickets(ctx)
		if err != nil {
			t.Fatalf("NeedsAttentionTickets failed: %v", err)
		}
		if len(got) != 1 || got[0].Ticket.ID != held.ID {
			t.Fatalf("list = %+v, want only the held ticket %s", got, held.ID)
		}
		row := got[0]
		if row.LatestAssignment.ID != heldA.ID || row.LatestAssignment.Status != store.AssignmentStatusRunning {
			t.Fatalf("latest assignment = %+v, want the running %s", row.LatestAssignment, heldA.ID)
		}
		if row.LastEvent != "ticket.needs_attention" {
			t.Fatalf("last event = %q, want the hold reason event", row.LastEvent)
		}
		if row.WaitsForPerson {
			t.Fatalf("waitsForPerson = true, want false while the latest assignment is running (在途不定格)")
		}
		_ = live
	})

	t.Run("Given a held ticket whose assignment settled unresolved and nobody spoke after, when the admin list runs, assert that waitsForPerson is true (AC: 等人工单带谓词标记)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, a := adminTicketFixture(t, s, "github:acme/api", "issue-comment:22", "issue:44")
		dispatchTask(t, s, a, "task-22")
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     a.ID,
			AssignmentStatus: store.AssignmentStatusUnresolved,
			TicketID:         tk.ID,
			TicketStatus:     store.TicketStatusNeedsAttention,
		}); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		got, err := s.NeedsAttentionTickets(ctx)
		if err != nil {
			t.Fatalf("NeedsAttentionTickets failed: %v", err)
		}
		if len(got) != 1 || !got[0].WaitsForPerson {
			t.Fatalf("list = %+v, want the unresolved hold flagged waitsForPerson", got)
		}
	})
}

func TestRetryTicket(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a held ticket, when the admin retries it, assert that a fresh pending assignment is minted, the ticket runs again, and the action is audited (AC: retry)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, a := adminTicketFixture(t, s, "github:acme/api", "issue-comment:30", "issue:42")
		dispatchTask(t, s, a, "task-30")
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     a.ID,
			AssignmentStatus: store.AssignmentStatusUnresolved,
			TicketID:         tk.ID,
			TicketStatus:     store.TicketStatusNeedsAttention,
		}); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		minted, err := s.RetryTicket(ctx, tk.ID, "jerry")
		if err != nil {
			t.Fatalf("RetryTicket failed: %v", err)
		}
		if minted.Status != store.AssignmentStatusPending || minted.Kind != store.AssignmentKindFollowup {
			t.Fatalf("minted assignment = %+v, want a pending follow-up", minted)
		}
		if minted.ID == a.ID {
			t.Fatal("the retry must mint a NEW assignment, not resurrect the old one")
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusRunning {
			t.Fatalf("ticket = %q, want running after the retry", status)
		}
		events, err := s.AuditEvents(ctx, tk.ID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		last := events[len(events)-1]
		if last.Event != "admin.retry" || last.Detail["actor"] != "jerry" || last.Detail["assignment_id"] != minted.ID {
			t.Fatalf("audit = %+v, want admin.retry with the actor and the minted assignment", last)
		}
	})

	t.Run("Given a running ticket, when the admin retries it, assert that the action is refused (retry 只针对挂起工单)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, _ := adminTicketFixture(t, s, "github:acme/api", "issue-comment:31", "issue:42")

		_, err := s.RetryTicket(ctx, tk.ID, "jerry")
		if !errors.Is(err, store.ErrTicketNotHeld) {
			t.Fatalf("error = %v, want ErrTicketNotHeld", err)
		}
	})

	t.Run("Given an unknown ticket id, when the admin retries it, assert that the action fails with ErrTicketNotFound", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		if _, err := s.RetryTicket(ctx, "ticket-none", "jerry"); !errors.Is(err, store.ErrTicketNotFound) {
			t.Fatalf("error = %v, want ErrTicketNotFound", err)
		}
	})
}

func TestStopTicket(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a held ticket with a paused running assignment, when the admin stops it, assert that the ticket fails, the assignment settles dropped, and the action is audited (AC: stop)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, a := adminTicketFixture(t, s, "github:acme/api", "issue-comment:40", "issue:42")
		dispatchTask(t, s, a, "task-40")
		if _, err := s.MarkTicketNeedsAttention(ctx, tk.ID, "task paused in input-required"); err != nil {
			t.Fatalf("MarkTicketNeedsAttention failed: %v", err)
		}

		if err := s.StopTicket(ctx, tk.ID, "jerry"); err != nil {
			t.Fatalf("StopTicket failed: %v", err)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusFailed {
			t.Fatalf("ticket = %q, want failed after the stop", status)
		}
		assignments, err := s.Assignments(ctx, tk.ID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if assignments[0].Status != store.AssignmentStatusEnded || assignments[0].Report != "dropped" {
			t.Fatalf("assignment = %+v, want ended under dropped", assignments[0])
		}
		events, _ := s.AuditEvents(ctx, tk.ID)
		last := events[len(events)-1]
		if last.Event != "admin.stop" || last.Detail["actor"] != "jerry" {
			t.Fatalf("audit = %+v, want admin.stop with the actor", last)
		}
	})

	t.Run("Given a stopped ticket whose worker task later reports ready, when the late terminal report applies, assert that the ticket stays failed (迟到终态不复活已停工单)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, a := adminTicketFixture(t, s, "github:acme/api", "issue-comment:41", "issue:42")
		dispatchTask(t, s, a, "task-41")
		if err := s.StopTicket(ctx, tk.ID, "jerry"); err != nil {
			t.Fatalf("StopTicket failed: %v", err)
		}

		outcome, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     a.ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         tk.ID,
			TicketStatus:     store.TicketStatusEnded,
		})
		if err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}
		if outcome.Applied || outcome.TicketChanged {
			t.Fatalf("outcome = %+v, want the replay absorbed", outcome)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusFailed {
			t.Fatalf("ticket = %q, want it to stay failed", status)
		}
	})

	t.Run("Given an ended ticket, when the admin stops it, assert that the action is refused (终态工单不可 stop)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, a := adminTicketFixture(t, s, "github:acme/api", "issue-comment:42", "issue:42")
		dispatchTask(t, s, a, "task-42")
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     a.ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         tk.ID,
			TicketStatus:     store.TicketStatusEnded,
		}); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		if err := s.StopTicket(ctx, tk.ID, "jerry"); !errors.Is(err, store.ErrTicketTerminal) {
			t.Fatalf("error = %v, want ErrTicketTerminal", err)
		}
	})
}

func TestRecordFollowupInput(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a live owner ticket and a person's message on its thread, when the follow-up input is recorded, assert that the ignore decision and the owner's input audit land together (followupOwners: Engine 只许 stop，消息记为输入)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)
		tk, _ := adminTicketFixture(t, s, "github:acme/api", "issue-comment:50", "issue:42")

		dec, err := s.RecordFollowupInput(ctx, tk.ID, "github:acme/api", "issue-comment:50-followup", store.DecisionInput{
			Outcome:       store.DecisionIgnore,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "thread owned by " + tk.ID,
		})
		if err != nil {
			t.Fatalf("RecordFollowupInput failed: %v", err)
		}
		if dec.Outcome != store.DecisionIgnore {
			t.Fatalf("decision = %+v, want ignore", dec)
		}
		events, err := s.AuditEvents(ctx, tk.ID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		found := false
		for _, ev := range events {
			if ev.Event == "ticket.followup_input" && ev.Detail["deterministic_id"] == "issue-comment:50-followup" {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit trail = %+v, want a ticket.followup_input row for the trigger", events)
		}
	})

	t.Run("Given an unknown owner ticket, when the follow-up input is recorded, assert that nothing lands (未知识输入被拒绝)", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		if _, err := s.RecordFollowupInput(ctx, "ticket-none", "github:acme/api", "issue-comment:x", store.DecisionInput{
			Outcome:       store.DecisionIgnore,
			Strength:      "explicit",
			PolicyVersion: "abc123",
		}); err == nil {
			t.Fatal("recording a follow-up input on a missing owner must fail")
		}
	})
}
