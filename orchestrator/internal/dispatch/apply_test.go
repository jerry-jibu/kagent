package dispatch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// dispatchedFixture creates a store whose fixture assignment is running with
// a task bound, plus a Dispatcher over the fake transport.
func dispatchedFixture(t *testing.T) (*store.Store, string, string, *dispatch.Dispatcher) {
	t.Helper()
	s, _, assignment := acceptFixture(t)
	ctx := context.Background()
	if err := s.MarkAssignmentDispatched(ctx, assignment.ID, assignment.TicketID, "task-9", "ctx-9", time.Time{}); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}
	fake := a2a.NewFake()
	d := dispatch.New(s, fake, dispatch.Options{WorkerAgent: "kagent/worker", PushBaseURL: "http://orchestrator:8080"})
	return s, assignment.TicketID, assignment.ID, d
}

func TestDispatcherApplyTerminal(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a terminal task carrying a Report document, when it is applied, assert that all six report states drive the ticket machine (AC: Report 六态全覆盖)", func(t *testing.T) {
		scenarios := []struct {
			name             string
			report           string
			wantAssignment   string
			wantTicket       string
			wantTicketChange bool
		}{
			{"progress keeps the ticket running for the follow-up scan", `{"kind":"report","state":"progress","summary":"halfway"}`, store.AssignmentStatusEnded, store.TicketStatusRunning, false},
			{"ready ends the ticket", `{"kind":"report","state":"ready","artifacts":["patches/t1-1.patch"]}`, store.AssignmentStatusEnded, store.TicketStatusEnded, true},
			{"waiting moves the ticket to needs-attention", `{"kind":"report","state":"waiting","summary":"need input"}`, store.AssignmentStatusEnded, store.TicketStatusNeedsAttention, true},
			{"blocked moves the ticket to needs-attention", `{"kind":"report","state":"blocked"}`, store.AssignmentStatusEnded, store.TicketStatusNeedsAttention, true},
			{"failed fails the ticket", `{"kind":"report","state":"failed"}`, store.AssignmentStatusEnded, store.TicketStatusFailed, true},
			{"dropped moves the ticket to needs-attention", `{"kind":"report","state":"dropped"}`, store.AssignmentStatusEnded, store.TicketStatusNeedsAttention, true},
		}
		for _, sc := range scenarios {
			t.Run("Given "+sc.name+", when it is applied, assert that the assignment settles and the ticket lands on its state", func(t *testing.T) {
				s, ticketID, assignmentID, d := dispatchedFixture(t)

				applied, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-9", State: a2a.StateCompleted, StatusText: sc.report})
				if err != nil {
					t.Fatalf("ApplyTerminal failed: %v", err)
				}
				if !applied.AssignmentSettled {
					t.Fatalf("applied = %+v, want the assignment settled", applied)
				}

				assignments, err := s.Assignments(ctx, ticketID)
				if err != nil {
					t.Fatalf("Assignments failed: %v", err)
				}
				if assignments[0].Status != sc.wantAssignment {
					t.Fatalf("assignment status = %q, want %q", assignments[0].Status, sc.wantAssignment)
				}
				if assignments[0].Report == "" {
					t.Fatal("the report state must be recorded on the assignment (the follow-up scan keys off it)")
				}
				status, err := s.TicketStatus(ctx, ticketID)
				if err != nil {
					t.Fatalf("TicketStatus failed: %v", err)
				}
				if status != sc.wantTicket {
					t.Fatalf("ticket status = %q, want %q", status, sc.wantTicket)
				}
				if applied.TicketChanged != sc.wantTicketChange {
					t.Fatalf("ticket changed = %v, want %v", applied.TicketChanged, sc.wantTicketChange)
				}
				_ = assignmentID
			})
		}
	})

	t.Run("Given a terminal task with no readable report, when it is applied, assert that the bare A2A state maps to the fallback report (0001 §3: task 终态+文本提取)", func(t *testing.T) {
		scenarios := []struct{ state, want string }{
			{a2a.StateCompleted, store.TicketStatusEnded},
			{a2a.StateFailed, store.TicketStatusFailed},
			{a2a.StateCanceled, store.TicketStatusNeedsAttention},
			{a2a.StateRejected, store.TicketStatusNeedsAttention},
		}
		for _, sc := range scenarios {
			s, ticketID, _, d := dispatchedFixture(t)
			if _, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-9", State: sc.state, StatusText: "the agent ended its turn"}); err != nil {
				t.Fatalf("ApplyTerminal(%s) failed: %v", sc.state, err)
			}
			status, _ := s.TicketStatus(ctx, ticketID)
			if status != sc.want {
				t.Fatalf("task state %s → ticket %q, want %q", sc.state, status, sc.want)
			}
		}
	})

	t.Run("Given a task paused in input-required, when it is applied, assert that the reserved detection point moves the ticket to needs-attention while the assignment keeps running (0002 §3, P0 waiting→needs-attention)", func(t *testing.T) {
		s, ticketID, _, d := dispatchedFixture(t)

		applied, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-9", State: a2a.StateInputRequired, StatusText: "which approach do you prefer?"})
		if err != nil {
			t.Fatalf("ApplyTerminal failed: %v", err)
		}
		if applied.AssignmentSettled {
			t.Fatal("an input-required task is not terminal; the assignment must keep running")
		}
		if !applied.TicketChanged {
			t.Fatal("the detection point must move the ticket")
		}
		assignments, _ := s.Assignments(ctx, ticketID)
		if assignments[0].Status != store.AssignmentStatusRunning {
			t.Fatalf("assignment = %q, want still running", assignments[0].Status)
		}
		if status, _ := s.TicketStatus(ctx, ticketID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want needs-attention", status)
		}
	})

	t.Run("Given the same terminal task applied twice (push then reconcile), when it is applied again, assert that the replay is a no-op (幂等)", func(t *testing.T) {
		s, ticketID, _, d := dispatchedFixture(t)
		first, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-9", State: a2a.StateCompleted, StatusText: `{"kind":"report","state":"ready"}`})
		if err != nil || !first.AssignmentSettled {
			t.Fatalf("first apply = %+v, %v", first, err)
		}

		second, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-9", State: a2a.StateCompleted, StatusText: `{"kind":"report","state":"ready"}`})
		if err != nil {
			t.Fatalf("replay failed: %v", err)
		}
		if second.AssignmentSettled || second.TicketChanged {
			t.Fatalf("replay = %+v, want a no-op", second)
		}
		events, _ := s.AuditEvents(ctx, ticketID)
		count := 0
		for _, ev := range events {
			if ev.Event == "report.ready" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("report.ready audit rows = %d, want exactly 1", count)
		}
	})

	t.Run("Given a task id no assignment carries, when it is applied, assert that ErrUnknownTask surfaces (the push receiver's 404)", func(t *testing.T) {
		_, _, _, d := dispatchedFixture(t)
		if _, err := d.ApplyTerminal(ctx, a2a.Task{ID: "task-foreign", State: a2a.StateCompleted}); !errors.Is(err, dispatch.ErrUnknownTask) {
			t.Fatalf("err = %v, want ErrUnknownTask", err)
		}
	})
}
