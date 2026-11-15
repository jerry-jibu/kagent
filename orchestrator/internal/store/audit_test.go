package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// testPoolForSchema opens a direct pool pinned to the store's schema, for
// test seeding only. The Store keeps its pool private; tests must not
// widen the production API to write fixtures.
func testPoolForSchema(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open seed pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedTicket inserts one ticket row with the given status.
func seedTicket(t *testing.T, s *store.Store, id, status string) {
	t.Helper()
	pool := testPoolForSchema(t, s.Schema())
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tickets (id, thread_source, thread_key, mandate, status)
		 VALUES ($1, 'github', $2, '{"policy_version":"test"}', $3)`,
		id, fmt.Sprintf("key-%s", id), status); err != nil {
		t.Fatalf("seed ticket %s: %v", id, err)
	}
}

func TestFailTicket(t *testing.T) {
	t.Run("Given a migrated store with an accepted ticket, when FailTicket runs, assert that the ticket status becomes failed and an audit row records the failure with its reason", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		seedTicket(t, s, "ticket-f1", store.TicketStatusPending)

		err := s.FailTicket(ctx, "ticket-f1", "shallow clone failed: remote branch gone")
		if err != nil {
			t.Fatalf("FailTicket failed: %v", err)
		}

		status, err := s.TicketStatus(ctx, "ticket-f1")
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != "failed" {
			t.Fatalf("ticket status = %q, want failed", status)
		}

		events, err := s.AuditEvents(ctx, "ticket-f1")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("audit events = %d, want exactly the failure record", len(events))
		}
		if events[0].Event != "ticket.failed" {
			t.Fatalf("audit event = %q, want ticket.failed", events[0].Event)
		}
		if events[0].Detail["reason"] != "shallow clone failed: remote branch gone" {
			t.Fatalf("audit detail = %v, want the failure reason", events[0].Detail)
		}
	})

	t.Run("Given no ticket row for the id, when FailTicket runs, assert that ErrTicketNotFound is returned and no audit row is written", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		err := s.FailTicket(ctx, "ghost-ticket", "clone failed")
		if !errors.Is(err, store.ErrTicketNotFound) {
			t.Fatalf("error = %v, want ErrTicketNotFound", err)
		}

		events, err := s.AuditEvents(ctx, "ghost-ticket")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("audit events = %d, want none for a nonexistent ticket", len(events))
		}
	})

	t.Run("Given an already failed ticket, when FailTicket runs again, assert that the replay is a no-op with NO second audit record — an audit trail never describes a change that did not happen", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		seedTicket(t, s, "ticket-f2", store.TicketStatusPending)

		if err := s.FailTicket(ctx, "ticket-f2", "first failure"); err != nil {
			t.Fatalf("first FailTicket failed: %v", err)
		}
		if err := s.FailTicket(ctx, "ticket-f2", "second failure"); err != nil {
			t.Fatalf("second FailTicket failed: %v", err)
		}

		events, err := s.AuditEvents(ctx, "ticket-f2")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("audit events = %d, want exactly one (the replay wrote no row)", len(events))
		}
	})

	t.Run("Given an ENDED ticket (delivered its PR), when FailTicket runs, assert the ended state stands — a delivered ticket can never be rewritten to failed (PR #11 review, Required)", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		seedTicket(t, s, "ticket-f3", store.TicketStatusEnded)

		if err := s.FailTicket(ctx, "ticket-f3", "late blame attempt"); err != nil {
			t.Fatalf("FailTicket on an ended ticket must be a no-op, not an error: %v", err)
		}
		if status, _ := s.TicketStatus(ctx, "ticket-f3"); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want ended to stand against the failure write", status)
		}
		events, _ := s.AuditEvents(ctx, "ticket-f3")
		if len(events) != 0 {
			t.Fatalf("audit events = %d, want none for the refused write", len(events))
		}
	})

	t.Run("Given a ticket with a live pending assignment, when FailTicket runs, assert the assignment settles with the ticket in the same transaction — no dangling assignment (PR #11 review, Required)", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		tk, assignment := adminTicketFixture(t, s, "github:acme/api", "issue-comment:9100", "issue:91")

		if err := s.FailTicket(ctx, tk.ID, "materialization failed"); err != nil {
			t.Fatalf("FailTicket failed: %v", err)
		}
		got, err := s.Assignments(ctx, tk.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("Assignments = %+v, %v", got, err)
		}
		if got[0].Status != store.AssignmentStatusEnded {
			t.Fatalf("assignment = %q, want settled (ended) with the failed ticket", got[0].Status)
		}
		if got[0].ID != assignment.ID {
			t.Fatalf("settled assignment %s is not the fixture's %s", got[0].ID, assignment.ID)
		}
	})
}

func TestRecordAudit(t *testing.T) {
	t.Run("Given a migrated store, when RecordAudit appends an event, assert that it is readable back with its detail and ordering is preserved", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}
		seedTicket(t, s, "ticket-a1", store.TicketStatusPending)

		if err := s.RecordAudit(ctx, "ticket-a1", "workspace.materialized", map[string]any{
			"workspace": "ticket-a1", "base_commit": "abc123", "files": 42,
		}); err != nil {
			t.Fatalf("RecordAudit failed: %v", err)
		}
		if err := s.RecordAudit(ctx, "ticket-a1", "dispatch.sent", map[string]any{
			"task_id": "task-1",
		}); err != nil {
			t.Fatalf("RecordAudit failed: %v", err)
		}

		events, err := s.AuditEvents(ctx, "ticket-a1")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("audit events = %d, want 2 in insertion order", len(events))
		}
		if events[0].Event != "workspace.materialized" || events[1].Event != "dispatch.sent" {
			t.Fatalf("audit order = [%s, %s], want [workspace.materialized, dispatch.sent]",
				events[0].Event, events[1].Event)
		}
		if events[0].Detail["base_commit"] != "abc123" {
			t.Fatalf("audit detail = %v, want the recorded payload", events[0].Detail)
		}
		if events[0].CreatedAt.IsZero() {
			t.Fatal("audit event has no timestamp")
		}
	})
}
