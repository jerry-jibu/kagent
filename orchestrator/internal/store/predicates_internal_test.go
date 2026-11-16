package store

// Internal tests for the two consistency predicates of docs/designs 0001 §3:
// waitsForPerson and followupOwners. The scans observe interleavings of
// assignments, triggers and drifted ticket rows that the public mutation API
// deliberately cannot produce on demand (the Engine is the only writer), so
// the fixture seeds raw rows the way real transitions leave them. Behavior of
// the mutations themselves is covered by the external store tests.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// testCtx is the background context of the predicate tests.
func testCtx() context.Context { return context.Background() }

// newPredicateStore opens a migrated store on an isolated schema.
func newPredicateStore(t *testing.T) *Store {
	t.Helper()
	s := openInternalTestStore(t)
	mustMigrateInternal(t, s)
	return s
}

// createAcceptedTicket runs the real accept path: one ticket with its initial
// pending assignment and confirmation effect. Each call needs a distinct
// (source, deterministic id) — the accept-once decision index enforces it.
func createAcceptedTicket(t *testing.T, s *Store, source, deterministicID, threadKey string) Ticket {
	t.Helper()
	created, err := s.CreateTicket(testCtx(), TicketInput{
		Source:          source,
		DeterministicID: deterministicID,
		ThreadKey:       threadKey,
		Decision: DecisionInput{
			Outcome:       DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "test request",
		},
		Mandate: Mandate{
			PolicyVersion: "abc123",
			Strength:      "explicit",
			Sender:        "alice",
			Scope:         MandateScope{Repo: "acme/api", Issue: 42},
		},
		Effects: []OutboxEffect{{
			Channel:        "github",
			Address:        "acme/api#42",
			IdempotencyKey: "confirm:" + TicketIDForTrigger(source, deterministicID),
			Payload:        []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"body":"ack"}`),
		}},
	})
	if err != nil {
		t.Fatalf("CreateTicket failed: %v", err)
	}
	return created.Ticket
}

// initialAssignment returns the ticket's single assignment (fresh accept).
func initialAssignment(t *testing.T, s *Store, ticketID string) Assignment {
	t.Helper()
	assignments, err := s.Assignments(testCtx(), ticketID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("Assignments = %+v, %v; want the single initial assignment", assignments, err)
	}
	return assignments[0]
}

// dispatchAssignment walks the assignment to running the way the dispatcher
// does (task bound, deadline set).
func dispatchAssignment(t *testing.T, s *Store, a Assignment, taskID string) {
	t.Helper()
	if err := s.MarkAssignmentDispatched(testCtx(), a.ID, a.TicketID, taskID, "ctx-"+taskID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}
}

// settleAssignment settles a running assignment under a report the way
// ApplyTerminal's plans do.
func settleAssignment(t *testing.T, s *Store, ticketID, assignmentID, assignmentStatus, report, ticketStatus string) {
	t.Helper()
	outcome, err := s.ApplyAssignmentTerminal(testCtx(), TerminalPlan{
		AssignmentID:     assignmentID,
		AssignmentStatus: assignmentStatus,
		ReportState:      report,
		TicketID:         ticketID,
		TicketStatus:     ticketStatus,
	})
	if err != nil {
		t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
	}
	if !outcome.Applied {
		t.Fatal("ApplyAssignmentTerminal replayed a fresh settle")
	}
}

// ingestPersonComment ingests one person comment trigger on the thread after
// the given moment — the "person spoke" event of the predicates.
func ingestPersonComment(t *testing.T, s *Store, source, threadKey, deterministicID string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond) // the ingest must land strictly after the settle instant
	_, err := s.IngestEvents(testCtx(), SourceRef{Name: source, Kind: "github"}, []Event{{
		ThreadKey:       threadKey,
		DeterministicID: deterministicID,
		Payload:         []byte(fmt.Sprintf(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"comment_id":1,"author":"alice","body":"hello","url":"u","created_at":%q}`, time.Now().UTC().Format(time.RFC3339))),
		OccurredAt:      time.Now().UTC(),
	}}, time.Hour)
	if err != nil {
		t.Fatalf("IngestEvents failed: %v", err)
	}
}

// singleCandidate asserts the scan returned exactly one candidate for the
// given ticket and returns it.
func singleCandidate(t *testing.T, got []WaitsForPersonTicket, ticketID string) WaitsForPersonTicket {
	t.Helper()
	if len(got) != 1 || got[0].TicketID != ticketID {
		t.Fatalf("candidates = %+v, want exactly ticket %s", got, ticketID)
	}
	return got[0]
}

func TestWaitsForPersonTickets(t *testing.T) {
	const source = "github:acme/api"

	t.Run("Given a held ticket whose assignment settled blocked with nobody speaking after, when the waitsForPerson scan runs, assert that the ticket is a candidate (0001 §3: blocked → 自动输入挂起)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:1", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-1")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "blocked", TicketStatusNeedsAttention)

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		c := singleCandidate(t, got, tk.ID)
		if c.Report != "blocked" || c.AssignmentStatus != AssignmentStatusEnded {
			t.Fatalf("candidate = %+v, want the blocked ended assignment", c)
		}
	})

	t.Run("Given a person spoke on the thread after the blocked settle, when the waitsForPerson scan runs, assert that the ticket is not a candidate (有人发言 → 谓词不成立)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:2", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-2")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "blocked", TicketStatusNeedsAttention)
		ingestPersonComment(t, s, source, "issue:42", "issue-comment:2-followup")

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		for _, c := range got {
			if c.TicketID == tk.ID {
				t.Fatalf("candidates = %+v, want no candidate once a person spoke", got)
			}
		}
	})

	t.Run("Given a retry minted a fresh pending assignment after the blocked settle, when the waitsForPerson scan runs, assert that the ticket is not a candidate (最新 Assignment 未终结 → 工单在途)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:3", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-3")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "blocked", TicketStatusNeedsAttention)

		// Simulate the person's retry decision: the ticket runs again and a
		// fresh follow-up assignment is minted (the admin retry lands this
		// atomically in T8; the predicate must see the same shape).
		if _, err := s.pool.Exec(testCtx(),
			"UPDATE tickets SET status = $2 WHERE id = $1", tk.ID, TicketStatusRunning); err != nil {
			t.Fatalf("failed to seed the retried ticket: %v", err)
		}
		if _, err := s.CreateFollowupAssignment(testCtx(), tk.ID); err != nil {
			t.Fatalf("CreateFollowupAssignment failed: %v", err)
		}

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		for _, c := range got {
			if c.TicketID == tk.ID {
				t.Fatalf("candidates = %+v, want no candidate while the latest assignment is pending", got)
			}
		}
	})

	t.Run("Given the latest assignment is unresolved, when the waitsForPerson scan runs, assert that the ticket is a candidate (红线: unresolved 挂起等人)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:4", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-4")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusUnresolved, "", TicketStatusNeedsAttention)

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		c := singleCandidate(t, got, tk.ID)
		if c.AssignmentStatus != AssignmentStatusUnresolved {
			t.Fatalf("candidate = %+v, want the unresolved assignment", c)
		}
	})

	t.Run("Given the latest assignment settled failed on a ticket that still reads live (drifted state), when the waitsForPerson scan runs, assert that the ticket is a candidate (0001 §3: failed → 挂起输入)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:5", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-5")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "failed", TicketStatusFailed)
		// Drift: the ticket reads running again (hand-repaired or future
		// transition) while its latest settled outcome is a failed report.
		if _, err := s.pool.Exec(testCtx(),
			"UPDATE tickets SET status = $2 WHERE id = $1", tk.ID, TicketStatusRunning); err != nil {
			t.Fatalf("failed to seed the drifted ticket: %v", err)
		}

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		singleCandidate(t, got, tk.ID)
	})

	t.Run("Given a ticket whose work ended ready, when the waitsForPerson scan runs, assert that the ended ticket is not a candidate (终态工单不参与谓词)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:6", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-6")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "ready", TicketStatusEnded)

		got, err := s.WaitsForPersonTickets(testCtx())
		if err != nil {
			t.Fatalf("WaitsForPersonTickets failed: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("candidates = %+v, want none for an ended ticket", got)
		}
	})
}

func TestFollowupOwnerOfThread(t *testing.T) {
	const source = "github:acme/api"

	t.Run("Given a thread holding a live ticket, when the followupOwner is resolved, assert that the live ticket owns the thread's follow-up (0001 §3: followupOwners)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:10", "issue:42")

		owner, found, err := s.FollowupOwnerOfThread(testCtx(), source, "issue:42")
		if err != nil {
			t.Fatalf("FollowupOwnerOfThread failed: %v", err)
		}
		if !found || owner.ID != tk.ID {
			t.Fatalf("owner = %+v found=%v, want ticket %s", owner, found, tk.ID)
		}
	})

	t.Run("Given the thread's only ticket ended, when the followupOwner is resolved, assert that the thread has no owner (终态工单不持有 follow-up)", func(t *testing.T) {
		s := newPredicateStore(t)
		tk := createAcceptedTicket(t, s, source, "issue-comment:11", "issue:42")
		a := initialAssignment(t, s, tk.ID)
		dispatchAssignment(t, s, a, "task-11")
		settleAssignment(t, s, tk.ID, a.ID, AssignmentStatusEnded, "ready", TicketStatusEnded)

		owner, found, err := s.FollowupOwnerOfThread(testCtx(), source, "issue:42")
		if err != nil {
			t.Fatalf("FollowupOwnerOfThread failed: %v", err)
		}
		if found {
			t.Fatalf("owner = %+v, want none for an ended ticket", owner)
		}
	})

	t.Run("Given two tickets on one thread with the older ended, when the followupOwner is resolved, assert that the newest live ticket owns the follow-up (最新活工单持有)", func(t *testing.T) {
		s := newPredicateStore(t)
		old := createAcceptedTicket(t, s, source, "issue-comment:12", "issue:42")
		a := initialAssignment(t, s, old.ID)
		dispatchAssignment(t, s, a, "task-12")
		settleAssignment(t, s, old.ID, a.ID, AssignmentStatusEnded, "ready", TicketStatusEnded)

		time.Sleep(20 * time.Millisecond) // distinct created_at ordering
		fresh := createAcceptedTicket(t, s, source, "issue-comment:13", "issue:42")

		owner, found, err := s.FollowupOwnerOfThread(testCtx(), source, "issue:42")
		if err != nil {
			t.Fatalf("FollowupOwnerOfThread failed: %v", err)
		}
		if !found || owner.ID != fresh.ID {
			t.Fatalf("owner = %+v found=%v, want the newest live ticket %s", owner, found, fresh.ID)
		}
	})
}
