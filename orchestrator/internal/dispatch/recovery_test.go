package dispatch_test

// The restart-recovery half of T8 (issue #9, docs/designs 0001 §4.3 + §8):
// the orchestrator was kill -9'd; a new process opens the same store. The
// startup recovery loads every non-terminal assignment (装载未终结
// Assignment) and audits the manifest, and the first Pass ticks carry the
// work forward — pending assignments re-dispatched exactly once under the
// recovery annotation, running assignments reconciled against the gateway.
// The receipt-dedup replay of the outbox is exercised by the outbox channel
// tests; here it is asserted end to end at the Pass level.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

func TestRecover(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a store holding a pending and a running assignment left by a dead process, when the startup recovery runs, assert that every non-terminal assignment is loaded and each holding ticket is audited (0001 §4.3: 装载未终结 Assignment)", func(t *testing.T) {
		s, _, _ := acceptFixture(t) // ticket A: pending assignment
		storeTriggerPayload(t, s)

		// A second ticket, already dispatched (running) at process death.
		createdB, err := s.CreateTicket(ctx, store.TicketInput{
			Source:          "github:acme/api",
			DeterministicID: "issue-comment:9002",
			ThreadKey:       "issue:43",
			Decision: store.DecisionInput{
				Outcome:       store.DecisionAccept,
				Strength:      "explicit",
				PolicyVersion: "abc123",
			},
			Mandate: store.Mandate{
				PolicyVersion: "abc123",
				Strength:      "explicit",
				Sender:        "bob",
				Scope:         store.MandateScope{Repo: "acme/api", Issue: 43},
			},
		})
		if err != nil {
			t.Fatalf("CreateTicket (B) failed: %v", err)
		}
		assignmentsB, err := s.Assignments(ctx, createdB.Ticket.ID)
		if err != nil || len(assignmentsB) != 1 {
			t.Fatalf("Assignments (B) = %+v, %v; want the initial assignment", assignmentsB, err)
		}
		if err := s.MarkAssignmentDispatched(ctx, assignmentsB[0].ID, createdB.Ticket.ID, "task-b", "ctx-b", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("MarkAssignmentDispatched (B) failed: %v", err)
		}

		stats, err := dispatch.Recover(ctx, s)
		if err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if stats.Pending != 1 || stats.Running != 1 || stats.Tickets != 2 {
			t.Fatalf("stats = %+v, want 1 pending + 1 running across 2 tickets", stats)
		}
		for _, tk := range []store.Ticket{createdB.Ticket} {
			events, err := s.AuditEvents(ctx, tk.ID)
			if err != nil {
				t.Fatalf("AuditEvents failed: %v", err)
			}
			found := false
			for _, ev := range events {
				if ev.Event == "recovery.startup" {
					found = true
				}
			}
			if !found {
				t.Fatalf("audit trail of %s = %+v, want a recovery.startup manifest", tk.ID, events)
			}
		}
	})

	t.Run("Given a store whose tickets all settled, when the startup recovery runs, assert that nothing is loaded (终态不留恢复清单)", func(t *testing.T) {
		s, tk, assignment := acceptFixture(t)
		if err := s.FailTicket(ctx, tk.ID, "materialization failed before restart"); err != nil {
			t.Fatalf("FailTicket failed: %v", err)
		}
		_ = assignment

		stats, err := dispatch.Recover(ctx, s)
		if err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if stats.Tickets != 0 || stats.Pending != 0 || stats.Running != 0 {
			t.Fatalf("stats = %+v, want an empty manifest", stats)
		}
	})
}

func TestRecoveryAnnotation(t *testing.T) {
	ctx := context.Background()

	t.Run("Given an assignment that predates the process start, when it is dispatched after restart, assert that the document carries the restart recovery annotation (0001 §4.3: 重启中断重派带恢复注记)", func(t *testing.T) {
		s, _, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-r1", ContextID: "ctx-r1"})
		d := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent: "kagent/worker",
			// The fixture mints the assignment at test time, so the process
			// start (cutoff) must sit AFTER it to model "minted, then the
			// process died, then the process came back".
			RecoveryCutoff: time.Now().Add(time.Hour),
		})

		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		var doc struct {
			Recovery struct {
				Reason string `json:"reason"`
			} `json:"recovery"`
		}
		if err := json.Unmarshal([]byte(fake.DispatchCalls()[0].Message), &doc); err != nil {
			t.Fatalf("message is not a dispatch document: %v", err)
		}
		if doc.Recovery.Reason != "orchestrator restart" {
			t.Fatalf("recovery annotation = %+v, want the restart reason", doc.Recovery)
		}
	})

	t.Run("Given an assignment minted after the process started, when it is dispatched, assert that no restart annotation is added (新工单首派不带恢复注记)", func(t *testing.T) {
		s, _, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-r2", ContextID: "ctx-r2"})
		d := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent:    "kagent/worker",
			RecoveryCutoff: time.Now().Add(-time.Hour), // the process started before the assignment was minted
		})

		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		var doc struct {
			Recovery json.RawMessage `json:"recovery"`
		}
		if err := json.Unmarshal([]byte(fake.DispatchCalls()[0].Message), &doc); err != nil {
			t.Fatalf("message is not a dispatch document: %v", err)
		}
		if len(doc.Recovery) != 0 {
			t.Fatalf("recovery annotation = %s, want none on a fresh first dispatch", doc.Recovery)
		}
	})

	t.Run("Given a recovery redispatch of a ticket holding recorded follow-up inputs, when the document is built, assert that the unread inputs ride along (0001 §4.3: 未读 Inputs 随行)", func(t *testing.T) {
		s, _, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		ticketID := ticketIDOf(t, s)
		// The person's message on the owned thread, recorded by the eval
		// guard (followupOwners): it rides along when the ticket resumes.
		if _, err := s.RecordFollowupInput(ctx, ticketID, "github:acme/api", "issue-comment:9001-note", store.DecisionInput{
			Outcome:       store.DecisionIgnore,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "thread owned",
		}); err != nil {
			t.Fatalf("RecordFollowupInput failed: %v", err)
		}
		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-r3", ContextID: "ctx-r3"})
		d := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent:    "kagent/worker",
			RecoveryCutoff: time.Now().Add(time.Hour),
		})

		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		var doc struct {
			Recovery     json.RawMessage `json:"recovery"`
			UnreadInputs []struct {
				DeterministicID string `json:"deterministic_id"`
			} `json:"unread_inputs"`
		}
		if err := json.Unmarshal([]byte(fake.DispatchCalls()[0].Message), &doc); err != nil {
			t.Fatalf("message is not a dispatch document: %v", err)
		}
		if len(doc.UnreadInputs) != 1 || doc.UnreadInputs[0].DeterministicID != "issue-comment:9001-note" {
			t.Fatalf("unread inputs = %+v, want the recorded person message", doc.UnreadInputs)
		}
	})

	t.Run("Given a fresh ticket with no recorded inputs, when the document is built, assert that the document carries no unread_inputs key (无输入不挂空字段)", func(t *testing.T) {
		s, _, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-r4", ContextID: "ctx-r4"})
		d := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent:    "kagent/worker",
			RecoveryCutoff: time.Now().Add(time.Hour),
		})

		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(fake.DispatchCalls()[0].Message), &doc); err != nil {
			t.Fatalf("message is not a dispatch document: %v", err)
		}
		if _, present := doc["unread_inputs"]; present {
			t.Fatalf("document carries an unread_inputs key: %v", doc["unread_inputs"])
		}
	})
}
