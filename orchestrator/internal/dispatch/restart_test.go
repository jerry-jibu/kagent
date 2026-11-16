package dispatch_test

// The kill -9 restart demonstration of T8 (issue #9 AC 1–2, docs/designs
// 0001 §8 orchestrator-crash row): the old process left its store state and
// died; a NEW process — new transports, new Pass, same store — runs
// dispatch.Recover and then Pass ticks. In-progress ticket state must be
// intact, the interrupted assignment re-dispatched exactly once, a duplicate
// outbox delivery absorbed by the receipt, and an uncertain worker outcome
// red-lined with no automatic replay.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// restartOptions is the new process's dispatcher configuration: the
// RecoveryCutoff stamps the restart instant on the DATABASE clock — the
// clock assignment created_at is written by, matching main.go.
func restartOptions(t *testing.T, s *store.Store) dispatch.Options {
	t.Helper()
	dbNow, err := s.Now(context.Background())
	if err != nil {
		t.Fatalf("failed to read the database clock: %v", err)
	}
	return dispatch.Options{
		WorkerAgent:    "kagent/worker",
		PushBaseURL:    "http://orchestrator:8080",
		RecoveryCutoff: dbNow,
	}
}

func TestRestartRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a dead process left a pending assignment and an undelivered confirmation effect, when the new process recovers and ticks, assert that the assignment is dispatched exactly once, the manifest audited, and nothing dispatches again (AC: 中断 Assignment 重派恰一次)", func(t *testing.T) {
		s, tk, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		// The dead process never got a tick in: no dispatch, no delivery.

		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-n1", ContextID: "ctx-n1"})
		gh := github.NewFake()
		t.Cleanup(gh.Close)
		newPass := dispatch.NewPass(s, dispatch.New(s, fake, restartOptions(t, s)),
			outbox.New(s, github.NewTransport(github.TransportConfig{BaseURL: gh.URL()})),
			dispatch.PassOptions{MaxFollowups: 3})

		if _, err := dispatch.Recover(ctx, s); err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		events, err := s.AuditEvents(ctx, tk.ID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		manifests := 0
		for _, ev := range events {
			if ev.Event == "recovery.startup" {
				manifests++
			}
		}
		if manifests != 1 {
			t.Fatalf("recovery.startup rows = %d, want the audited manifest", manifests)
		}

		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("first tick failed: %v", err)
		}
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("second tick failed: %v", err)
		}
		if got := len(fake.DispatchCalls()); got != 1 {
			t.Fatalf("dispatch calls = %d, want exactly one across the recovery ticks", got)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusRunning {
			t.Fatalf("ticket = %q, want running on the recovered dispatch", status)
		}
	})

	t.Run("Given the dead process posted the confirmation comment but died before the receipt, when the new process replays the effect, assert that the marker search absorbs the duplicate and the receipt records the match (AC: 重复投递被 receipt 吸收)", func(t *testing.T) {
		s, tk, _ := acceptFixture(t)
		// The old process's post landed on GitHub; the crash ate the receipt.
		gh := github.NewFake()
		t.Cleanup(gh.Close)
		oldTransport := github.NewTransport(github.TransportConfig{BaseURL: gh.URL()})
		marker := github.EffectMarker("confirm:" + tk.ID)
		if _, err := oldTransport.PostIssueComment(ctx, "acme/api", 42,
			"Acknowledged — opened ticket "+tk.ID+"\n"+marker); err != nil {
			t.Fatalf("simulated post failed: %v", err)
		}
		effects, err := s.Effects(ctx)
		if err != nil || len(effects) != 1 || effects[0].State != store.EffectPending {
			t.Fatalf("effects = %+v, %v; want the single pending effect", effects, err)
		}

		// The new process drains the outbox: the marker search finds the
		// comment, records the receipt, and never posts again.
		deliverer := outbox.New(s, github.NewTransport(github.TransportConfig{BaseURL: gh.URL()}))
		stats, err := deliverer.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Confirmed != 1 || stats.Delivered != 0 {
			t.Fatalf("stats = %+v, want the replay confirmed, not re-posted", stats)
		}
		// The conversation carries exactly the simulated pre-crash comment:
		// the replay added none.
		if posted := gh.PostedComments(); len(posted) != 1 {
			t.Fatalf("posted comments = %+v, want exactly the pre-crash one", posted)
		}
		effects, _ = s.Effects(ctx)
		if effects[0].State != store.EffectDelivered {
			t.Fatalf("effect state = %q, want delivered with the receipt", effects[0].State)
		}
		if !strings.Contains(string(effects[0].Receipt), "matched_marker") {
			t.Fatalf("receipt = %s, want the marker match recorded", effects[0].Receipt)
		}
	})

	t.Run("Given a running assignment whose task completed at the gateway but whose push was lost in the crash, when the new process recovers and ticks, assert that the terminal outcome is backfilled exactly once (AC: 进行中工单状态无损)", func(t *testing.T) {
		s, tk, assignment := acceptFixture(t)
		storeTriggerPayload(t, s)
		if err := s.MarkAssignmentDispatched(ctx, assignment.ID, tk.ID, "task-lost", "ctx-lost", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}

		fake := a2a.NewFake()
		fake.ScriptTaskState("task-lost", a2a.Task{
			ID: "task-lost", State: a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"the work finished while the orchestrator was down"}`,
		})
		newPass := dispatch.NewPass(s, dispatch.New(s, fake, restartOptions(t, s)), nil, dispatch.PassOptions{MaxFollowups: 3})

		// State survives the crash untouched: still running, unjudged.
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusRunning {
			t.Fatalf("ticket before recovery = %q, want the committed running state intact", status)
		}

		if _, err := dispatch.Recover(ctx, s); err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("first tick failed: %v", err)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want the lost terminal report backfilled", status)
		}

		// The next tick re-observes the same terminal state: a replay, absorbed.
		fake.ScriptTaskState("task-lost", a2a.Task{
			ID: "task-lost", State: a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready"}`,
		})
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("second tick failed: %v", err)
		}
		events, _ := s.AuditEvents(ctx, tk.ID)
		readyReports := 0
		for _, ev := range events {
			if ev.Event == "report.ready" {
				readyReports++
			}
		}
		if readyReports != 1 {
			t.Fatalf("report.ready rows = %d, want exactly one (幂等对账)", readyReports)
		}
	})

	t.Run("Given a running assignment whose worker crashed during a remote tool call and the task is gone, when the new process recovers and ticks, assert that the assignment is red-lined unresolved with the ticket held and nothing replays (AC: 远程调用中崩溃 → unresolved + needs-attention，无自动重放)", func(t *testing.T) {
		s, tk, assignment := acceptFixture(t)
		storeTriggerPayload(t, s)
		if err := s.MarkAssignmentDispatched(ctx, assignment.ID, tk.ID, "task-dead", "ctx-dead", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}

		fake := a2a.NewFake()
		fake.FailTaskState("task-dead", a2a.ErrTaskNotFound)
		newPass := dispatch.NewPass(s, dispatch.New(s, fake, restartOptions(t, s)), nil, dispatch.PassOptions{MaxFollowups: 3})

		if _, err := dispatch.Recover(ctx, s); err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		stats, err := newPass.Tick(ctx)
		if err != nil {
			t.Fatalf("first tick failed: %v", err)
		}
		if stats.Redlined != 1 {
			t.Fatalf("stats = %+v, want the vanished task red-lined", stats)
		}
		assignments, err := s.Assignments(ctx, tk.ID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if assignments[0].Status != store.AssignmentStatusUnresolved {
			t.Fatalf("assignment = %q, want unresolved (结局不可判定，红线)", assignments[0].Status)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want needs-attention", status)
		}
		events, _ := s.AuditEvents(ctx, tk.ID)
		found := false
		for _, ev := range events {
			if ev.Event == "assignment.unresolved" {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit trail = %+v, want the red-line record", events)
		}

		// The red line holds: further ticks neither re-dispatch nor resume.
		for i := 0; i < 3; i++ {
			if _, err := newPass.Tick(ctx); err != nil {
				t.Fatalf("tick %d failed: %v", i+2, err)
			}
		}
		if got := len(fake.DispatchCalls()); got != 0 {
			t.Fatalf("dispatch calls = %d, want none after the red line (不自动重放)", got)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusNeedsAttention {
			t.Fatalf("ticket = %q, want the hold to persist", status)
		}
		if got := len(fake.TaskStateCalls()); got != 1 {
			t.Fatalf("task queries = %d, want exactly one (the assignment settled; later ticks move on)", got)
		}
	})

	t.Run("Given a recovery redispatch of an assignment minted before the restart, when the new process dispatches it, assert that the document carries the restart annotation and the follow-up inputs ride along (0001 §4.3: 恢复注记 + 未读 Inputs)", func(t *testing.T) {
		s, tk, _ := acceptFixture(t)
		storeTriggerPayload(t, s)
		if _, err := s.RecordFollowupInput(ctx, tk.ID, "github:acme/api", "issue-comment:9001-input", store.DecisionInput{
			Outcome:       store.DecisionIgnore,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "person spoke while the orchestrator was down",
		}); err != nil {
			t.Fatalf("RecordFollowupInput failed: %v", err)
		}

		fake := a2a.NewFake()
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-n2", ContextID: "ctx-n2"})
		newPass := dispatch.NewPass(s, dispatch.New(s, fake, restartOptions(t, s)), nil, dispatch.PassOptions{MaxFollowups: 3})

		if _, err := dispatch.Recover(ctx, s); err != nil {
			t.Fatalf("Recover failed: %v", err)
		}
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("tick failed: %v", err)
		}
		calls := fake.DispatchCalls()
		if len(calls) != 1 {
			t.Fatalf("dispatch calls = %d, want the recovery redispatch", len(calls))
		}
		if !strings.Contains(calls[0].Message, `"reason":"orchestrator restart"`) {
			t.Fatalf("dispatch document = %s, want the restart recovery annotation", calls[0].Message)
		}
		if !strings.Contains(calls[0].Message, "issue-comment:9001-input") {
			t.Fatalf("dispatch document = %s, want the unread input riding along", calls[0].Message)
		}
	})

	t.Run("Given a vanished worker task, when the admin retries the held ticket, assert that a fresh assignment resumes the work while the unresolved assignment stays settled (人决策后续：重试=新 Assignment)", func(t *testing.T) {
		s, tk, assignment := acceptFixture(t)
		storeTriggerPayload(t, s)
		if err := s.MarkAssignmentDispatched(ctx, assignment.ID, tk.ID, "task-dead2", "ctx-dead2", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}

		fake := a2a.NewFake()
		fake.FailTaskState("task-dead2", a2a.ErrTaskNotFound)
		newPass := dispatch.NewPass(s, dispatch.New(s, fake, restartOptions(t, s)), nil, dispatch.PassOptions{MaxFollowups: 3})
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("red-line tick failed: %v", err)
		}

		minted, err := s.RetryTicket(ctx, tk.ID, "jerry")
		if err != nil {
			t.Fatalf("RetryTicket failed: %v", err)
		}
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-fresh", ContextID: "ctx-dead2"})
		if _, err := newPass.Tick(ctx); err != nil {
			t.Fatalf("post-retry tick failed: %v", err)
		}
		if len(fake.DispatchCalls()) != 1 {
			t.Fatalf("dispatch calls = %d, want exactly the retry dispatch", len(fake.DispatchCalls()))
		}
		assignments, err := s.Assignments(ctx, tk.ID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if len(assignments) != 2 || assignments[0].Status != store.AssignmentStatusUnresolved || assignments[1].ID != minted.ID {
			t.Fatalf("assignments = %+v, want the settled unresolved one plus the fresh retry", assignments)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusRunning {
			t.Fatalf("ticket = %q, want running on the person's retry", status)
		}
	})
}
