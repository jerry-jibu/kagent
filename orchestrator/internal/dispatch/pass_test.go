package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// passFixture wires a Pass over the store and fake transport; the fixture
// assignment is running with task-9 bound (dispatchedFixture).
func passFixture(t *testing.T) (*store.Store, string, *a2a.Fake, *dispatch.Pass) {
	t.Helper()
	s, ticketID, _, _ := dispatchedFixture(t)
	storeTriggerPayload(t, s)
	fake := a2a.NewFake()
	pass := dispatch.NewPass(s, dispatch.New(s, fake, dispatch.Options{WorkerAgent: "kagent/worker"}), nil,
		dispatch.PassOptions{MaxFollowups: 3})
	return s, ticketID, fake, pass
}

// passFixtureWithPending wires a Pass against a fresh undispatched ticket.
func passFixtureWithPending(t *testing.T) (*store.Store, *a2a.Fake, *dispatch.Pass) {
	t.Helper()
	s, _, _ := acceptFixture(t)
	storeTriggerPayload(t, s)
	fake := a2a.NewFake()
	pass := dispatch.NewPass(s, dispatch.New(s, fake, dispatch.Options{WorkerAgent: "kagent/worker"}), nil,
		dispatch.PassOptions{MaxFollowups: 3})
	return s, fake, pass
}

// passFixtureWithPendingAndDeliverer wires a Pass with the outbox deliverer
// over a ticket carrying one pending confirmation effect.
func passFixtureWithPendingAndDeliverer(t *testing.T) (*store.Store, *a2a.Fake, *dispatch.Pass) {
	t.Helper()
	s, _, _ := acceptFixture(t) // creates the ticket with its confirmation effect
	storeTriggerPayload(t, s)
	fake := a2a.NewFake()
	d := dispatch.New(s, fake, dispatch.Options{WorkerAgent: "kagent/worker"})
	gh := github.NewFake()
	t.Cleanup(gh.Close)
	ghTransport := github.NewTransport(github.TransportConfig{BaseURL: gh.URL()})
	pass := dispatch.NewPass(s, d, outbox.New(s, ghTransport), dispatch.PassOptions{})
	return s, fake, pass
}

// assignmentIDOf reads the ticket's (single) assignment id.
func assignmentIDOf(t *testing.T, s *store.Store, ticketID string) string {
	t.Helper()
	assignments, err := s.Assignments(context.Background(), ticketID)
	if err != nil || len(assignments) == 0 {
		t.Fatalf("Assignments = %+v, %v", assignments, err)
	}
	return assignments[0].ID
}

func TestPassTickReconcile(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a running assignment whose push was swallowed, when one Pass tick reconciles, assert that the terminal task state is backfilled within the tick (AC: 吞掉 push → ≤2 节拍补记账)", func(t *testing.T) {
		s, ticketID, fake, pass := passFixture(t)
		// The task is terminal at the gateway; NO push was ever delivered.
		fake.ScriptTaskState("task-9", a2a.Task{
			ID: "task-9", ContextID: "ctx-9", State: a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"done"}`,
		})

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Reconciled != 1 {
			t.Fatalf("reconciled = %d, want 1", stats.Reconciled)
		}

		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want ended by the backfilled terminal state", status)
		}
		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusEnded || assignments[0].Report != dispatch.ReportReady {
			t.Fatalf("assignment = %+v, want ended/ready", assignments[0])
		}

		// The second tick is a no-op (幂等): nothing left to reconcile.
		stats, err = pass.Tick(ctx)
		if err != nil {
			t.Fatalf("second Tick failed: %v", err)
		}
		if stats.Reconciled != 0 || stats.Redlined != 0 {
			t.Fatalf("second tick stats = %+v, want nothing left", stats)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		count := 0
		for _, ev := range events {
			if ev.Event == "report.ready" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("report.ready rows = %d, want exactly 1 across both ticks", count)
		}
	})

	t.Run("Given a running assignment whose task vanished at the gateway, when one Pass tick reconciles, assert that the assignment is marked unresolved and the ticket moves to needs-attention without any redispatch (AC/红线: 不确定结局不自动重放)", func(t *testing.T) {
		s, ticketID, fake, pass := passFixture(t)
		fake.FailTaskState("task-9", a2a.ErrTaskNotFound)

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Redlined != 1 {
			t.Fatalf("redlined = %d, want 1", stats.Redlined)
		}

		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusUnresolved {
			t.Fatalf("assignment = %q, want unresolved (红线标记)", assignments[0].Status)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want needs-attention", status)
		}
		if len(fake.DispatchCalls()) != 0 {
			t.Fatal("the red line forbids automatic redispatch of an unresolved outcome")
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		if events[len(events)-1].Event != "assignment.unresolved" {
			t.Fatalf("last audit = %q, want assignment.unresolved", events[len(events)-1].Event)
		}
	})

	t.Run("Given a gateway that cannot answer, when one Pass tick reconciles, assert that the assignment stays running for the next tick and the failure is counted, not fatal", func(t *testing.T) {
		s, ticketID, fake, pass := passFixture(t)
		fake.FailTaskState("task-9", errors.New("gateway 503"))

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick must not fail the whole beat on one gateway error: %v", err)
		}
		if stats.ReconcileFailed != 1 {
			t.Fatalf("reconcile failed = %d, want 1", stats.ReconcileFailed)
		}
		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusRunning {
			t.Fatalf("assignment = %q, want still running (unknown state is not the red line)", assignments[0].Status)
		}
	})
}

func TestPassTickDispatchAndFollowup(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a pending assignment on a live ticket, when one Pass tick runs, assert that the tick dispatches it (the Pass owns the beat the T4 deliverer left)", func(t *testing.T) {
		_, fake, pass := passFixtureWithPending(t)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: "ctx-1"})

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Dispatched != 1 {
			t.Fatalf("dispatched = %d, want 1", stats.Dispatched)
		}
		if len(fake.DispatchCalls()) != 1 {
			t.Fatalf("dispatch calls = %d, want 1", len(fake.DispatchCalls()))
		}
	})

	t.Run("Given a running ticket whose only assignment ended under a progress report, when one Pass tick runs, assert that a follow-up assignment is minted and dispatched in the same tick (AC: 到期重派)", func(t *testing.T) {
		s, ticketID, fake, pass := passFixture(t)
		// The first assignment settles under progress — the follow-up queue.
		_, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignmentIDOf(t, s, ticketID),
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      dispatch.ReportProgress,
			TicketID:         ticketID,
		})
		if err != nil {
			t.Fatalf("progress apply failed: %v", err)
		}
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-10", ContextID: "ctx-9"})

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Followups != 1 || stats.Dispatched != 1 {
			t.Fatalf("stats = %+v, want one follow-up minted and dispatched", stats)
		}

		assignments, _ := s.Assignments(ctx, ticketID)
		if len(assignments) != 2 {
			t.Fatalf("assignments = %d, want the follow-up added", len(assignments))
		}
		followup := assignments[1]
		if followup.Kind != store.AssignmentKindFollowup || followup.Status != store.AssignmentStatusRunning {
			t.Fatalf("follow-up = %+v, want a running followup", followup)
		}
		if fake.DispatchCalls()[0].ContextID != "ctx-9" {
			t.Fatalf("follow-up contextId = %q, want the ticket session (续发 contextId 不变)", fake.DispatchCalls()[0].ContextID)
		}
	})

	t.Run("Given a ticket that spent its follow-up cap, when one Pass tick runs, assert that no further assignment is minted (unbounded progress loops cannot burn worker turns)", func(t *testing.T) {
		s, ticketID, fake, pass := passFixture(t)
		first := assignmentIDOf(t, s, ticketID)
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID: first, AssignmentStatus: store.AssignmentStatusEnded,
			ReportState: dispatch.ReportProgress, TicketID: ticketID,
		}); err != nil {
			t.Fatalf("progress apply failed: %v", err)
		}
		// Cap is 3 in the fixture: spend all three follow-ups. Each lands a
		// distinct task id — the store binds one task id to one assignment.
		for i := range 3 {
			fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: fmt.Sprintf("task-f%d", i), ContextID: "ctx-9"})
			if _, err := pass.Tick(ctx); err != nil {
				t.Fatalf("tick %d failed: %v", i, err)
			}
			assignments, _ := s.Assignments(ctx, ticketID)
			last := assignments[len(assignments)-1]
			if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
				AssignmentID: last.ID, AssignmentStatus: store.AssignmentStatusEnded,
				ReportState: dispatch.ReportProgress, TicketID: ticketID,
			}); err != nil {
				t.Fatalf("progress apply %d failed: %v", i, err)
			}
		}

		assignments, _ := s.Assignments(ctx, ticketID)
		if len(assignments) != 4 { // initial + 3 follow-ups
			t.Fatalf("assignments = %d, want initial + 3 follow-ups", len(assignments))
		}
		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("capped Tick failed: %v", err)
		}
		if stats.Followups != 0 {
			t.Fatalf("followups = %d, want 0 at the cap", stats.Followups)
		}
		after, _ := s.Assignments(ctx, ticketID)
		if len(after) != 4 {
			t.Fatalf("assignments = %d, want no mint past the cap", len(after))
		}
	})

	t.Run("Given a committed outbox effect, when one Pass tick runs with a deliverer wired, assert that the tick drains the outbox (the Pass takes over the T4 beat)", func(t *testing.T) {
		s, _, pass := passFixtureWithPendingAndDeliverer(t)

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Delivered != 1 {
			t.Fatalf("delivered = %d, want the pending effect drained", stats.Delivered)
		}
		effects, err := s.Effects(context.Background())
		if err != nil || len(effects) != 1 || effects[0].State != store.EffectDelivered {
			t.Fatalf("effects = %+v, %v; want the confirmation delivered", effects, err)
		}
	})
}
