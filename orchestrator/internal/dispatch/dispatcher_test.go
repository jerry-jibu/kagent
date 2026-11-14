package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
)

// acceptFixture creates one accepted ticket (with its pending assignment and
// confirmation effect) and returns the store, ticket and assignment.
func acceptFixture(t *testing.T) (*store.Store, store.Ticket, store.Assignment) {
	t.Helper()
	ctx := context.Background()
	s := testenv.NewMigratedStore(t)
	ticketID := store.TicketIDForTrigger("github:acme/api", "issue-comment:9001")
	created, err := s.CreateTicket(ctx, store.TicketInput{
		Source:          "github:acme/api",
		DeterministicID: "issue-comment:9001",
		ThreadKey:       "issue:42",
		Decision: store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "authorized coding request",
		},
		Mandate: store.Mandate{
			PolicyVersion: "abc123",
			Strength:      "explicit",
			Sender:        "alice",
			Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
		},
		Effects: []store.OutboxEffect{{
			Channel:        "github",
			Address:        "acme/api#42",
			IdempotencyKey: "confirm:" + ticketID,
			Payload:        []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"body":"Acknowledged"}`),
		}},
	})
	if err != nil {
		t.Fatalf("CreateTicket failed: %v", err)
	}
	assignments, err := s.Assignments(ctx, created.Ticket.ID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("Assignments = %+v, %v; want the single pending assignment", assignments, err)
	}
	return s, created.Ticket, assignments[0]
}

// fixtureTriggerPayload is the CommentPayload the fixture's trigger carries —
// the dispatch document embeds it as the original request.
var fixtureTriggerPayload = []byte(`{"kind":"github-issue-comment","repo":"acme/api","issue":42,"comment_id":9001,"author":"alice","body":"please fix the flaky test","url":"https://github.com/acme/api/issues/42#issuecomment-9001","created_at":"2026-01-01T00:00:00Z"}`)

// storeTriggerPayload ingests the fixture trigger so the dispatch document
// can carry the original request.
func storeTriggerPayload(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	_, err := s.IngestEvents(ctx, store.SourceRef{Name: "github:acme/api", Kind: "github"}, []store.Event{{
		ThreadKey:       "issue:42",
		DeterministicID: "issue-comment:9001",
		Payload:         fixtureTriggerPayload,
		OccurredAt:      time.Now().UTC(),
	}}, time.Hour)
	if err != nil {
		t.Fatalf("IngestEvents failed: %v", err)
	}
}

// ticketIDOf reads the fixture's ticket id (single-ticket store).
func ticketIDOf(t *testing.T, s *store.Store) string {
	t.Helper()
	tickets, err := s.Tickets(context.Background())
	if err != nil || len(tickets) == 0 {
		t.Fatalf("tickets = %+v, %v", tickets, err)
	}
	return tickets[0].ID
}

// dispatcherFixture wires a Dispatcher against the fake transport.
func dispatcherFixture(t *testing.T) (*store.Store, *a2a.Fake, *dispatch.Dispatcher) {
	t.Helper()
	s, _, _ := acceptFixture(t)
	fake := a2a.NewFake()
	d := dispatch.New(s, fake, dispatch.Options{
		WorkerAgent:  "kagent/worker",
		PushBaseURL:  "http://orchestrator:8080",
		MaxFollowups: 3,
	})
	return s, fake, d
}

func TestDispatcherDispatchPending(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a pending assignment, when the dispatch cycle runs, assert that the task identity is registered immediately and the ticket runs with the session context (AC: 派工后立即拿到 task id 并登记)", func(t *testing.T) {
		s, fake, d := dispatcherFixture(t)
		storeTriggerPayload(t, s)
		ticketID := ticketIDOf(t, s)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: "ctx-1", State: a2a.StateSubmitted})

		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		if stats.Dispatched != 1 || stats.Failed != 0 {
			t.Fatalf("stats = %+v, want one dispatch", stats)
		}

		assignments, err := s.Assignments(ctx, ticketID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if assignments[0].Status != store.AssignmentStatusRunning || assignments[0].TaskID == nil || *assignments[0].TaskID != "task-1" {
			t.Fatalf("assignment = %+v, want running with task-1", assignments[0])
		}
		got, err := s.Ticket(ctx, ticketID)
		if err != nil {
			t.Fatalf("Ticket failed: %v", err)
		}
		if got.Status != store.TicketStatusRunning || got.SessionID != "ctx-1" {
			t.Fatalf("ticket = %+v, want running with session ctx-1", got)
		}
	})

	t.Run("Given the dispatch request, when it is built, assert that the message is the dispatch document with the workspace, mandate, original request and push registration (0001 §4.1 任务文本 + workspace 名 + Mandate 摘要)", func(t *testing.T) {
		s, fake, d := dispatcherFixture(t)
		storeTriggerPayload(t, s)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: "ctx-1"})

		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}

		calls := fake.DispatchCalls()
		if len(calls) != 1 {
			t.Fatalf("dispatch calls = %d, want 1", len(calls))
		}
		call := calls[0]
		if call.Agent != "kagent/worker" {
			t.Fatalf("agent = %q, want the configured worker", call.Agent)
		}
		if call.PushURL != "http://orchestrator:8080/a2a/push" {
			t.Fatalf("push URL = %q, want the registered receiver endpoint", call.PushURL)
		}
		if !strings.HasPrefix(call.PushID, "push-assign-") {
			t.Fatalf("push id = %q, want the deterministic per-assignment registration", call.PushID)
		}

		var doc struct {
			Kind      string `json:"kind"`
			Ticket    string `json:"ticket"`
			Workspace string `json:"workspace"`
			Mandate   struct {
				Sender   string `json:"sender"`
				Strength string `json:"strength"`
				Scope    struct {
					Repo  string `json:"repo"`
					Issue int    `json:"issue"`
				} `json:"scope"`
			} `json:"mandate"`
			Request struct {
				Body string `json:"body"`
			} `json:"request"`
			Recovery json.RawMessage `json:"recovery"`
		}
		if err := json.Unmarshal([]byte(call.Message), &doc); err != nil {
			t.Fatalf("message is not a dispatch document: %v\n%s", err, call.Message)
		}
		if doc.Kind != "dispatch" || doc.Ticket == "" {
			t.Fatalf("document header = %s/%s, want kind dispatch with the ticket id", doc.Kind, doc.Ticket)
		}
		if doc.Workspace != doc.Ticket {
			t.Fatalf("workspace = %q, want the per-ticket workspace named by the ticket id (0001 D3)", doc.Workspace)
		}
		if doc.Mandate.Sender != "alice" || doc.Mandate.Scope.Repo != "acme/api" {
			t.Fatalf("mandate = %+v, want the ticket's mandate summary", doc.Mandate)
		}
		if doc.Request.Body != "please fix the flaky test" {
			t.Fatalf("request = %+v, want the original request", doc.Request)
		}
		if len(doc.Recovery) != 0 {
			t.Fatalf("recovery annotation = %s, want none on a first dispatch", doc.Recovery)
		}
	})

	t.Run("Given a dispatch that failed once, when the next cycle retries it, assert that the retry carries the recovery annotation and lands (AC: 到期重派带恢复注记)", func(t *testing.T) {
		s, fake, d := dispatcherFixture(t)
		storeTriggerPayload(t, s)
		fake.FailDispatch("kagent/worker", errors.New("gateway timed out"))

		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending (failing) returned an error: %v", err)
		}
		if stats.Failed != 1 || stats.Dispatched != 0 {
			t.Fatalf("stats = %+v, want one recorded failure", stats)
		}

		// The worker stays pending; the next cycle retries with the note.
		fake.FailDispatch("kagent/worker", nil) // the gateway recovered
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-2", ContextID: "ctx-2"})
		stats, err = d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending (retry) failed: %v", err)
		}
		if stats.Dispatched != 1 {
			t.Fatalf("retry stats = %+v, want one dispatch", stats)
		}

		var doc struct {
			Recovery struct {
				Attempt       int    `json:"attempt"`
				PreviousError string `json:"previous_error"`
			} `json:"recovery"`
		}
		if err := json.Unmarshal([]byte(fake.DispatchCalls()[1].Message), &doc); err != nil {
			t.Fatalf("retry message is not a dispatch document: %v", err)
		}
		if doc.Recovery.Attempt != 2 || doc.Recovery.PreviousError != "gateway timed out" {
			t.Fatalf("recovery annotation = %+v, want attempt 2 with the recorded error (恢复注记)", doc.Recovery)
		}
	})

	t.Run("Given a ticket that already ran a session, when a follow-up assignment is dispatched, assert that the same contextId travels on the message (AC: 同一 ticket 续发消息 contextId 不变)", func(t *testing.T) {
		s, fake, d := dispatcherFixture(t)
		storeTriggerPayload(t, s)
		ticketID := ticketIDOf(t, s)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: "ctx-1"})
		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("first dispatch failed: %v", err)
		}

		if _, err := s.CreateFollowupAssignment(ctx, ticketID); err != nil {
			t.Fatalf("CreateFollowupAssignment failed: %v", err)
		}
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-2", ContextID: "ctx-1"})
		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("follow-up dispatch failed: %v", err)
		}

		calls := fake.DispatchCalls()
		if len(calls) != 2 {
			t.Fatalf("dispatch calls = %d, want 2", len(calls))
		}
		if calls[1].ContextID != "ctx-1" {
			t.Fatalf("follow-up contextId = %q, want the unchanged ticket session ctx-1", calls[1].ContextID)
		}
	})
}
