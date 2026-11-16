package a2a_test

import (
	"errors"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

func TestFakeDispatchAndTaskState(t *testing.T) {
	t.Run("Given a scripted dispatch result, when a worker is dispatched, assert that the result comes back and the call is recorded verbatim", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.ScriptDispatch("kagent/worker", a2a.DispatchResult{
			TaskID:    "task-1",
			ContextID: "ctx-1",
			State:     a2a.StateSubmitted,
		})

		got, err := f.Dispatch(t.Context(), a2a.DispatchRequest{
			Agent:   "kagent/worker",
			Message: "the dispatch document",
			PushURL: "http://orchestrator:8080/a2a/push",
			PushID:  "push-1",
		})
		if err != nil {
			t.Fatalf("Dispatch failed: %v", err)
		}
		if got.TaskID != "task-1" || got.ContextID != "ctx-1" {
			t.Fatalf("result = %+v, want the scripted task identity", got)
		}

		calls := f.DispatchCalls()
		if len(calls) != 1 {
			t.Fatalf("dispatch calls = %d, want 1", len(calls))
		}
		if calls[0].Message != "the dispatch document" || calls[0].PushURL != "http://orchestrator:8080/a2a/push" || calls[0].PushID != "push-1" {
			t.Fatalf("recorded call = %+v, want the verbatim request", calls[0])
		}
		if calls[0].ContextID != "" {
			t.Fatalf("first dispatch must not carry a contextId, got %q", calls[0].ContextID)
		}
	})

	t.Run("Given a dispatch carrying a contextId, when it is recorded, assert that the session continuity key travels with the call", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-2", ContextID: "ctx-1", State: a2a.StateCompleted})

		if _, err := f.Dispatch(t.Context(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "continue", ContextID: "ctx-1"}); err != nil {
			t.Fatalf("Dispatch failed: %v", err)
		}
		if f.DispatchCalls()[0].ContextID != "ctx-1" {
			t.Fatalf("contextId = %q, want the ticket session's contextId", f.DispatchCalls()[0].ContextID)
		}
	})

	t.Run("Given a scripted dispatch failure, when a worker is dispatched, assert that the error surfaces (the unresponsive-worker path)", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.FailDispatch("kagent/worker", errors.New("gateway timed out"))

		if _, err := f.Dispatch(t.Context(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"}); err == nil {
			t.Fatal("a scripted dispatch failure must surface")
		}
	})

	t.Run("Given a scripted task state, when a task is queried, assert that the task comes back and the query is recorded", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.ScriptTaskState("task-1", a2a.Task{ID: "task-1", ContextID: "ctx-1", State: a2a.StateCompleted, StatusText: `{"kind":"report","state":"ready"}`})

		task, err := f.TaskState(t.Context(), "kagent/worker", "task-1")
		if err != nil {
			t.Fatalf("TaskState failed: %v", err)
		}
		if task.State != a2a.StateCompleted || task.StatusText == "" {
			t.Fatalf("task = %+v, want the scripted terminal task", task)
		}
		calls := f.TaskStateCalls()
		if len(calls) != 1 || calls[0].TaskID != "task-1" || calls[0].Agent != "kagent/worker" {
			t.Fatalf("task state calls = %+v, want the recorded query", calls)
		}
	})

	t.Run("Given a task missing at the gateway, when it is queried, assert that ErrTaskNotFound surfaces (the red-line trigger)", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.FailTaskState("task-gone", a2a.ErrTaskNotFound)

		if _, err := f.TaskState(t.Context(), "kagent/worker", "task-gone"); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("Given an unscripted agent or task, when dispatch or query runs, assert that the fake fails loudly instead of inventing results", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		if _, err := f.Dispatch(t.Context(), a2a.DispatchRequest{Agent: "kagent/worker", Message: "x"}); err == nil {
			t.Fatal("unscripted dispatch must fail")
		}
		if _, err := f.TaskState(t.Context(), "kagent/worker", "task-x"); err == nil {
			t.Fatal("unscripted task query must fail")
		}
	})
}

func TestTaskStateHelpers(t *testing.T) {
	t.Run("Given the A2A task states, when terminality is asked, assert that only the immutable states are terminal", func(t *testing.T) {
		terminal := []string{a2a.StateCompleted, a2a.StateFailed, a2a.StateCanceled, a2a.StateRejected}
		for _, s := range terminal {
			if !a2a.IsTerminalTaskState(s) {
				t.Fatalf("state %q must be terminal", s)
			}
		}
		nonTerminal := []string{a2a.StateSubmitted, a2a.StateWorking, a2a.StateInputRequired, a2a.StateAuthRequired, a2a.StateUnspecified, ""}
		for _, s := range nonTerminal {
			if a2a.IsTerminalTaskState(s) {
				t.Fatalf("state %q must not be terminal", s)
			}
		}
	})
}
