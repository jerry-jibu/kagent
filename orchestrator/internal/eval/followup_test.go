package eval_test

// The followupOwners guard of the eval chain (T8, issue #9; docs/designs
// 0001 §3: 还欠人回复的 Ticket 持有 Thread 上的 follow-up，Engine 只许 stop;
// §4.2: 该 Thread 有活 Ticket → 订阅扇出免评估). P0 has no fan-out channel,
// so a gate-allowed message on an owned thread is recorded as the owner's
// follow-up input and judged ignore — never forked into a second ticket.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// commentWithID builds an issue-comment payload with a distinct comment id,
// so each seeded trigger is a distinct trigger row on the thread.
func commentWithID(author, body string, commentID int64) intake.CommentPayload {
	pl := comment(author, body, "acme/api")
	pl.CommentID = commentID
	return pl
}

// seedAnotherTrigger ingests one more trigger on the fixture store — unlike
// seedTrigger it tolerates a store that already holds triggers.
func (f *engineFixture) seedAnotherTrigger(t *testing.T, pl intake.CommentPayload) (string, store.Trigger) {
	t.Helper()
	ctx := context.Background()
	payload, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("failed to encode trigger payload: %v", err)
	}
	source := github.SourceName(pl.Repo)
	if _, err := f.store.IngestEvents(ctx,
		store.SourceRef{Name: source, Kind: "github"},
		[]store.Event{{
			ThreadKey:       fmt.Sprintf("issue:%d", pl.Issue),
			DeterministicID: fmt.Sprintf("issue-comment:%d", pl.CommentID),
			Payload:         payload,
			OccurredAt:      pl.CreatedAt,
		}}, intake.DefaultSightline); err != nil {
		t.Fatalf("failed to ingest trigger: %v", err)
	}
	return source, f.mustTrigger(t, source, fmt.Sprintf("issue-comment:%d", pl.CommentID))
}

func TestEngineFollowupOwners(t *testing.T) {
	t.Run("Given a live ticket owns the thread, when a second gate-allowed trigger arrives on the same thread, assert that it is judged ignore without an evaluator invoke, recorded as the owner's follow-up input, and no second ticket is minted (Engine 只许 stop)", func(t *testing.T) {
		f := newEngineFixture(t)
		source, first := f.seedTrigger(t, commentWithID("alice", "@acme-bot please fix it", 1001))
		f.acceptEcho("authorized coding request")
		if _, err := f.engine.EvaluateTrigger(context.Background(), source, first, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}

		_, second := f.seedAnotherTrigger(t, commentWithID("alice", "@acme-bot also update the changelog", 1002))
		dec, err := f.engine.EvaluateTrigger(context.Background(), source, second, f.pol)
		if err != nil {
			t.Fatalf("second evaluation failed: %v", err)
		}
		if dec.Outcome != store.DecisionIgnore {
			t.Fatalf("decision = %q, want ignore (thread owned)", dec.Outcome)
		}
		tickets := f.tickets(t)
		if len(tickets) != 1 {
			t.Fatalf("tickets = %+v, want no fork beyond the owner", tickets)
		}
		if got := f.transport.CallCount("evaluator"); got != 1 {
			t.Fatalf("evaluator invokes = %d, want only the first trigger's (免评估)", got)
		}
		events, err := f.store.AuditEvents(context.Background(), tickets[0].ID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		found := false
		for _, ev := range events {
			if ev.Event == "ticket.followup_input" && ev.Detail["deterministic_id"] == second.DeterministicID {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit trail = %+v, want the follow-up input recorded on the owner", events)
		}
	})

	t.Run("Given the thread's ticket ended, when a fresh trigger arrives on the thread, assert that evaluation runs normally and the accept mints a new ticket (终态后线程自由)", func(t *testing.T) {
		f := newEngineFixture(t)
		source, first := f.seedTrigger(t, commentWithID("alice", "@acme-bot please fix it", 1101))
		f.acceptEcho("authorized coding request")
		if _, err := f.engine.EvaluateTrigger(context.Background(), source, first, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}
		tickets := f.tickets(t)
		if err := f.store.FailTicket(context.Background(), tickets[0].ID, "test teardown of the owner"); err != nil {
			t.Fatalf("FailTicket failed: %v", err)
		}

		_, second := f.seedAnotherTrigger(t, commentWithID("alice", "@acme-bot try again please", 1102))
		f.acceptEcho("authorized coding request")
		dec, err := f.engine.EvaluateTrigger(context.Background(), source, second, f.pol)
		if err != nil {
			t.Fatalf("second evaluation failed: %v", err)
		}
		if dec.Outcome != store.DecisionAccept {
			t.Fatalf("decision = %q, want a fresh accept on the freed thread", dec.Outcome)
		}
		if len(f.tickets(t)) != 2 {
			t.Fatalf("tickets = %+v, want the second ticket", f.tickets(t))
		}
	})

	t.Run("Given a live owner and a gate-denied trigger on the thread, when judged, assert that the decision is denied (the gate refuses before the owner guard)", func(t *testing.T) {
		f := newEngineFixture(t)
		source, first := f.seedTrigger(t, commentWithID("alice", "@acme-bot please fix it", 1201))
		f.acceptEcho("authorized coding request")
		if _, err := f.engine.EvaluateTrigger(context.Background(), source, first, f.pol); err != nil {
			t.Fatalf("first evaluation failed: %v", err)
		}

		// A sender the admission table does not know: the gate denies.
		_, second := f.seedAnotherTrigger(t, commentWithID("mallory", "unauthorized request", 1202))
		dec, err := f.engine.EvaluateTrigger(context.Background(), source, second, f.pol)
		if err != nil {
			t.Fatalf("second evaluation failed: %v", err)
		}
		if dec.Outcome != store.DecisionDenied {
			t.Fatalf("decision = %q, want the gate's denial", dec.Outcome)
		}
	})
}
