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

// Write-ahead dispatch intent (PR #11 C3): a dispatch whose binding write
// was lost to a crash must never read as a fresh first attempt. The restart
// recovery adopts the orphan task observation (after verifying it at the
// gateway), an attempted-but-unbound resend waits out the quarantine, a
// landed send that cannot bind is recorded — never silently retried — and
// every send carries a deterministic per-attempt message id.
func TestDispatchIntent(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a landed send whose binding write failed (simulated crash), when the restarted dispatcher ticks, assert it adopts the recorded task and applies its outcome instead of re-sending", func(t *testing.T) {
		s, fake, tk, assign, d := intentFixtureWith(t, func(st *store.Store) dispatch.Options {
			return restartOptions(t, st)
		})

		// Simulate the mid-flight death: the write-ahead intent counted the
		// send (attempts=1, no failure recorded) before the process died.
		if _, err := s.BeginDispatchAttempt(ctx, assign.ID); err != nil {
			t.Fatalf("BeginDispatchAttempt failed: %v", err)
		}

		// The crashed send landed task-orphan-1 at the gateway; the binding
		// write never committed. What survives: the pending assignment and
		// the orphan observation recorded at the unbound moment.
		if err := s.RecordOrphanTask(ctx, store.OrphanTask{
			Key:        assign.ID,
			TaskID:     "task-orphan-1",
			State:      a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"landed before the crash"}`,
		}); err != nil {
			t.Fatalf("RecordOrphanTask failed: %v", err)
		}
		fake.ScriptTaskState("task-orphan-1", a2a.Task{
			ID:         "task-orphan-1",
			ContextID:  tk.SessionID,
			State:      a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"landed before the crash"}`,
		})

		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		if stats.Dispatched != 1 {
			t.Fatalf("Dispatched = %d, want the adopted assignment counted as dispatched (stats %+v)", stats.Dispatched, stats)
		}
		if calls := fake.DispatchCalls(); len(calls) != 0 {
			t.Fatalf("a crash-recovered dispatch must adopt, not re-send: %d send calls recorded", len(calls))
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want the adopted task's ready report applied (ended)", status)
		}
		assignments, aerr := s.Assignments(ctx, tk.ID)
		if aerr != nil || len(assignments) != 1 {
			t.Fatalf("Assignments = %+v, %v", assignments, aerr)
		}
		if assignments[0].TaskID == nil || *assignments[0].TaskID != "task-orphan-1" {
			t.Fatalf("assignment task binding = %v, want task-orphan-1", assignments[0].TaskID)
		}
	})

	t.Run("Given an attempted-but-unbound assignment with no orphan observation, when the dispatcher ticks within the quarantine window, assert the resend is held; and with the window disabled, assert the resend carries the deterministic attempt message id", func(t *testing.T) {
		s, fake, tk, assign, d := intentFixtureWith(t, func(*store.Store) dispatch.Options {
			return dispatch.Options{
				WorkerAgent:  "kagent/worker",
				PushBaseURL:  "http://orchestrator:8080",
				MaxFollowups: 3,
				// One-minute quarantine: the fixture's failure is moments old.
				RedispatchQuarantine: time.Minute,
			}
		})

		// Attempt 1 dies mid-flight: the write-ahead intent counts the send
		// and no failure is recorded — a task may be live at the gateway.
		if _, err := s.BeginDispatchAttempt(ctx, assign.ID); err != nil {
			t.Fatalf("BeginDispatchAttempt failed: %v", err)
		}
		got, err := s.Assignments(ctx, tk.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("Assignments = %+v, %v", got, err)
		}
		if got[0].DispatchAttempts != 1 {
			t.Fatalf("DispatchAttempts = %d, want 1 (write-ahead intent counts the send)", got[0].DispatchAttempts)
		}

		// Attempt 2 within the quarantine: held, not sent, not a failure.
		fake.FailDispatch("kagent/worker", nil)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-2", ContextID: tk.SessionID})
		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("quarantined DispatchPending failed: %v", err)
		}
		if stats.Quarantined != 1 || stats.Dispatched != 0 {
			t.Fatalf("stats = %+v, want Quarantined=1 Dispatched=0 inside the window", stats)
		}
		if calls := fake.DispatchCalls(); len(calls) != 0 {
			t.Fatalf("a quarantined dispatch must not send; calls = %d", len(calls))
		}

		// The window expires: a dispatcher with the quarantine disabled
		// models the expired window instead of faking clocks.
		expired := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent:          "kagent/worker",
			PushBaseURL:          "http://orchestrator:8080",
			MaxFollowups:         3,
			RedispatchQuarantine: -1,
		})
		stats, err = expired.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("expired DispatchPending failed: %v", err)
		}
		if stats.Dispatched != 1 {
			t.Fatalf("Dispatched = %d, want the resend after the window expired", stats.Dispatched)
		}
		calls := fake.DispatchCalls()
		if len(calls) != 1 {
			t.Fatalf("send calls = %d, want the expired-window resend", len(calls))
		}
		want := dispatch.MessageIDFor(assign.ID, 2, "")
		if calls[0].MessageID != want {
			t.Fatalf("resend message id = %q, want the deterministic %q", calls[0].MessageID, want)
		}
	})

	t.Run("Given a send that timed out after the gateway may have accepted it, when the ticks follow, assert that no failure is recorded, the resend waits out the quarantine, and the expiry resends as attempt 2 (PR #11 review, Required: 模糊失败不得 fork worker)", func(t *testing.T) {
		s, fake, tk, assign, d := intentFixtureWith(t, func(*store.Store) dispatch.Options {
			return dispatch.Options{
				WorkerAgent:  "kagent/worker",
				PushBaseURL:  "http://orchestrator:8080",
				MaxFollowups: 3,
				// The failure is moments old: the window holds the resend.
				RedispatchQuarantine: time.Minute,
			}
		})

		// Ambiguous failure class: no answer arrived, so the gateway may
		// still be running (or may have landed) this send.
		fake.FailDispatch("kagent/worker", &a2a.AmbiguousDispatchError{
			Err: errors.New("context deadline exceeded"),
		})
		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending (ambiguous) failed: %v", err)
		}
		if stats.Ambiguous != 1 || stats.Failed != 0 || stats.Dispatched != 0 {
			t.Fatalf("stats = %+v, want one ambiguous attempt", stats)
		}

		// No failure on the ledger: a recorded last_error would license the
		// worker-forking immediate resend on the next tick.
		got, err := s.Assignments(ctx, tk.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("Assignments = %+v, %v", got, err)
		}
		if got[0].LastError != "" || got[0].DispatchAttempts != 1 {
			t.Fatalf("assignment ledger = attempts %d error %q, want attempt 1 with a clean error", got[0].DispatchAttempts, got[0].LastError)
		}
		events, err := s.AuditEvents(ctx, tk.ID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		ambiguous := 0
		for _, ev := range events {
			if ev.Event == "dispatch.ambiguous" {
				ambiguous++
			}
		}
		if ambiguous != 1 {
			t.Fatalf("dispatch.ambiguous rows = %d, want the ambiguity audited once", ambiguous)
		}

		// The immediate next tick holds the resend inside the quarantine
		// window: only adoption or the expiry may touch the send again.
		sendsBefore := len(fake.DispatchCalls())
		stats, err = d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("quarantined DispatchPending failed: %v", err)
		}
		if stats.Quarantined != 1 || stats.Dispatched != 0 {
			t.Fatalf("stats = %+v, want the resend quarantined", stats)
		}
		if len(fake.DispatchCalls()) != sendsBefore {
			t.Fatalf("a quarantined resend must not send; calls grew to %d", len(fake.DispatchCalls()))
		}

		// The window expires (modeled by the disabled quarantine): the
		// resend goes out as attempt 2 — its deterministic message id keeps
		// it correlatable even though the earlier send may still land.
		expired := dispatch.New(s, fake, dispatch.Options{
			WorkerAgent:          "kagent/worker",
			PushBaseURL:          "http://orchestrator:8080",
			MaxFollowups:         3,
			RedispatchQuarantine: -1,
		})
		fake.FailDispatch("kagent/worker", nil)
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-2", ContextID: "ctx-2"})
		stats, err = expired.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("expired DispatchPending failed: %v", err)
		}
		if stats.Dispatched != 1 {
			t.Fatalf("Dispatched = %d, want the resend after the window expired", stats.Dispatched)
		}
		calls := fake.DispatchCalls()
		if len(calls) != sendsBefore+1 {
			t.Fatalf("send calls = %d, want exactly the expired-window resend", len(calls))
		}
		want := dispatch.MessageIDFor(assign.ID, 2, "")
		if calls[len(calls)-1].MessageID != want {
			t.Fatalf("resend message id = %q, want the deterministic %q", calls[len(calls)-1].MessageID, want)
		}
	})

	t.Run("Given a push-recorded orphan observation keyed by the ticket session, when the attempted-but-unbound follow-up dispatches, assert that the session-keyed observation is adopted and bound instead of resent (PR #11 review, Required: adoption keys must collide)", func(t *testing.T) {
		s, fake, tk, _, d := intentFixtureWith(t, func(st *store.Store) dispatch.Options {
			return restartOptions(t, st)
		})

		// The first dispatch lands and binds the ticket session ctx-1.
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: "ctx-1"})
		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("first dispatch failed: %v", err)
		}

		// The assignment settles under progress; a follow-up is minted.
		assignments, err := s.Assignments(ctx, tk.ID)
		if err != nil || len(assignments) != 1 {
			t.Fatalf("Assignments = %+v, %v", assignments, err)
		}
		if _, err := s.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignments[0].ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      dispatch.ReportProgress,
			TicketID:         tk.ID,
		}); err != nil {
			t.Fatalf("progress apply failed: %v", err)
		}
		followup, err := s.CreateFollowupAssignment(ctx, tk.ID)
		if err != nil {
			t.Fatalf("CreateFollowupAssignment failed: %v", err)
		}

		// The kill -9 between the send and the bind: the write-ahead intent
		// counted the send, the send landed task-followup-1, and its push was
		// recorded for adoption under the SESSION key (RecordUnclaimedTask).
		if _, err := s.BeginDispatchAttempt(ctx, followup.ID); err != nil {
			t.Fatalf("BeginDispatchAttempt failed: %v", err)
		}
		if err := d.RecordUnclaimedTask(ctx, a2a.Task{
			ID:         "task-followup-1",
			ContextID:  "ctx-1",
			State:      a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"landed before the crash"}`,
		}); err != nil {
			t.Fatalf("RecordUnclaimedTask failed: %v", err)
		}
		fake.ScriptTaskState("task-followup-1", a2a.Task{
			ID:         "task-followup-1",
			ContextID:  "ctx-1",
			State:      a2a.StateCompleted,
			StatusText: `{"kind":"report","state":"ready","summary":"landed before the crash"}`,
		})

		// The next attempt adopts through the session key: no resend, the
		// landed task binds, and its ready report drives the ticket home.
		stats, err := d.DispatchPending(ctx)
		if err != nil {
			t.Fatalf("DispatchPending (adoption) failed: %v", err)
		}
		if stats.Dispatched != 1 || stats.Ambiguous != 0 {
			t.Fatalf("stats = %+v, want the adopted assignment counted as dispatched", stats)
		}
		if calls := fake.DispatchCalls(); len(calls) != 1 {
			t.Fatalf("send calls = %d, want only the original first dispatch (adoption must not re-send)", len(calls))
		}
		bound, err := s.Assignments(ctx, tk.ID)
		if err != nil || len(bound) != 2 {
			t.Fatalf("Assignments = %+v, %v", bound, err)
		}
		if bound[1].TaskID == nil || *bound[1].TaskID != "task-followup-1" {
			t.Fatalf("follow-up binding = %v, want task-followup-1 adopted via the session key", bound[1].TaskID)
		}
		if status, _ := s.TicketStatus(ctx, tk.ID); status != store.TicketStatusEnded {
			t.Fatalf("ticket = %q, want the adopted task's ready report applied", status)
		}
	})

	t.Run("Given a landed send whose binding could not commit, when the unbound path records the task, assert the observation is consumable exactly once and the audit row lands", func(t *testing.T) {
		st, tk, _ := acceptFixture(t)

		if err := st.RecordOrphanTask(ctx, store.OrphanTask{
			Key:        "assign-unbound-1",
			TaskID:     "task-unbound-1",
			State:      a2a.StateWorking,
			StatusText: "",
		}); err != nil {
			t.Fatalf("RecordOrphanTask failed: %v", err)
		}
		if err := st.RecordAudit(ctx, tk.ID, "dispatch.unbound", map[string]any{
			"assignment_id": "assign-x",
			"task_id":       "task-unbound-1",
			"error":         "store unavailable",
		}); err != nil {
			t.Fatalf("RecordAudit failed: %v", err)
		}

		obs, found, err := st.TakeOrphanTask(ctx, "assign-unbound-1")
		if err != nil || !found {
			t.Fatalf("TakeOrphanTask = %+v, %v, %v; want the recorded observation", obs, found, err)
		}
		if obs.TaskID != "task-unbound-1" || obs.State != a2a.StateWorking {
			t.Fatalf("orphan observation = %+v, want the recorded task", obs)
		}
		if _, found, _ := st.TakeOrphanTask(ctx, "assign-unbound-1"); found {
			t.Fatalf("a claimed observation must be consumed exactly once")
		}
		events, _ := st.AuditEvents(ctx, tk.ID)
		unbound := 0
		for _, ev := range events {
			if ev.Event == "dispatch.unbound" {
				unbound++
			}
		}
		if unbound != 1 {
			t.Fatalf("dispatch.unbound rows = %d, want 1", unbound)
		}
	})

	t.Run("Given a push event for a task no assignment is bound to, when the receiver serves it, assert the observation is recorded for adoption and the push is accepted", func(t *testing.T) {
		st, _, _, _, d := intentFixtureWith(t, func(inner *store.Store) dispatch.Options {
			return restartOptions(t, inner)
		})
		receiver := dispatch.NewReceiver(d, nil)

		rec := postPush(t, receiver, `{"statusUpdate":{"taskId":"task-push-orphan","contextId":"ctx-adopt-me","status":{"state":"`+a2a.StateCompleted+`","message":{"parts":[{"text":"done"}]}}}}`)
		if rec.Code != 200 {
			t.Fatalf("push status = %d, want 200 with the observation recorded (body: %s)", rec.Code, rec.Body.String())
		}

		obs, found, err := st.TakeOrphanTask(ctx, "ctx-adopt-me")
		if err != nil || !found {
			t.Fatalf("TakeOrphanTask = %+v, %v, %v; want the pushed task recorded", obs, found, err)
		}
		if obs.TaskID != "task-push-orphan" || obs.State != a2a.StateCompleted || obs.StatusText != "done" {
			t.Fatalf("orphan observation = %+v, want the pushed task verbatim", obs)
		}
	})

	t.Run("Given a first dispatch, when it lands, assert the message id carries the deterministic attempt id (attempt 1)", func(t *testing.T) {
		s, fake, tk, _, d := intentFixtureWith(t, func(*store.Store) dispatch.Options {
			return dispatch.Options{
				WorkerAgent: "kagent/worker",
				PushBaseURL: "http://orchestrator:8080",
			}
		})
		fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-1", ContextID: tk.SessionID})
		if _, err := d.DispatchPending(ctx); err != nil {
			t.Fatalf("DispatchPending failed: %v", err)
		}
		calls := fake.DispatchCalls()
		if len(calls) != 1 {
			t.Fatalf("send calls = %d, want 1", len(calls))
		}
		assignments, _ := s.Assignments(ctx, tk.ID)
		want := dispatch.MessageIDFor(assignments[0].ID, 1, "")
		if calls[0].MessageID != want {
			t.Fatalf("first-send message id = %q, want %q", calls[0].MessageID, want)
		}
	})
}

// intentFixture builds one pending assignment with its trigger payload
// ingested — the dispatcher-fixture shape with the fake returned. optsFrom
// receives the store so restart-style options can key off it.
func intentFixtureWith(t *testing.T, optsFrom func(*store.Store) dispatch.Options) (*store.Store, *a2a.Fake, store.Ticket, store.Assignment, *dispatch.Dispatcher) {
	t.Helper()
	st, tk, assignment := acceptFixture(t)
	storeTriggerPayload(t, st)
	fake := a2a.NewFake()
	return st, fake, tk, assignment, dispatch.New(st, fake, optsFrom(st))
}
