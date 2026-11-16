package dispatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// stuckFixture dispatches the fixture assignment with the given start
// deadline (G7: started_deadline = dispatch time + window) and wires a Pass.
func stuckFixture(t *testing.T, deadline time.Time) (*store.Store, string, *a2a.Fake, *dispatch.Pass) {
	t.Helper()
	ctx := context.Background()
	s, _, assignment := acceptFixture(t)
	if err := s.MarkAssignmentDispatched(ctx, assignment.ID, assignment.TicketID, "task-9", "ctx-9", deadline); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}
	storeTriggerPayload(t, s)
	fake := a2a.NewFake()
	pass := dispatch.NewPass(s, dispatch.New(s, fake, dispatch.Options{WorkerAgent: "kagent/worker"}), nil,
		dispatch.PassOptions{MaxFollowups: 3})
	return s, assignment.TicketID, fake, pass
}

func TestPassStuckDetection(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a dispatched assignment past its start deadline whose task never left working, when one Pass tick reconciles, assert that the ticket moves to needs-attention with a stuck audit and nothing is redispatched (G7: 受理后永不启动的滞留回收)", func(t *testing.T) {
		s, ticketID, fake, pass := stuckFixture(t, time.Now().Add(-time.Minute))
		fake.ScriptTaskState("task-9", a2a.Task{ID: "task-9", State: a2a.StateWorking})

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Stuck != 1 {
			t.Fatalf("stuck = %d, want 1", stats.Stuck)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want needs-attention", status)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		found := false
		for _, ev := range events {
			if ev.Event == "assignment.stuck" {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit trail = %+v, want an assignment.stuck row", events)
		}
		if len(fake.DispatchCalls()) != 0 {
			t.Fatal("a stuck assignment must not be automatically replayed (0009 红线)")
		}

		// The second tick does not duplicate the stuck audit (幂等).
		if _, err := pass.Tick(ctx); err != nil {
			t.Fatalf("second Tick failed: %v", err)
		}
		events, _ = s.AuditEvents(ctx, ticketID)
		count := 0
		for _, ev := range events {
			if ev.Event == "assignment.stuck" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("assignment.stuck rows = %d, want exactly 1", count)
		}
	})

	t.Run("Given a dispatched assignment inside its start deadline whose task is still working, when one Pass tick reconciles, assert that nothing moves (未超期不动)", func(t *testing.T) {
		s, ticketID, fake, pass := stuckFixture(t, time.Now().Add(time.Hour))
		fake.ScriptTaskState("task-9", a2a.Task{ID: "task-9", State: a2a.StateWorking})

		if _, err := pass.Tick(ctx); err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusRunning {
			t.Fatalf("ticket = %q, want still running inside the deadline", status)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		for _, ev := range events {
			if ev.Event == "assignment.stuck" {
				t.Fatal("no stuck audit may land inside the deadline")
			}
		}
	})

	t.Run("Given a dispatched assignment past its deadline whose task reached a terminal state, when one Pass tick reconciles, assert that the report path wins over the stuck branch (终态优先)", func(t *testing.T) {
		s, ticketID, fake, pass := stuckFixture(t, time.Now().Add(-time.Minute))
		fake.ScriptTaskState("task-9", a2a.Task{
			ID: "task-9", State: a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready"}`,
		})

		stats, err := pass.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick failed: %v", err)
		}
		if stats.Stuck != 0 || stats.Reconciled != 1 {
			t.Fatalf("stats = %+v, want the terminal backfill, not a stuck mark", stats)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want ended", status)
		}
	})
}
