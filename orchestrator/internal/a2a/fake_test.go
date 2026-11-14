package a2a_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
)

func TestFakeInvokeAgent(t *testing.T) {
	t.Run("Given a fake with a scripted response for an agent, when the agent is invoked, assert that the response is returned in order and the call is recorded verbatim", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.Script("evaluator", `{"outcome":"accept"}`, `{"outcome":"stop"}`)

		ctx := context.Background()
		first, err := f.InvokeAgent(ctx, "evaluator", "message one")
		if err != nil {
			t.Fatalf("first invoke failed: %v", err)
		}
		second, err := f.InvokeAgent(ctx, "evaluator", "message two")
		if err != nil {
			t.Fatalf("second invoke failed: %v", err)
		}
		if first != `{"outcome":"accept"}` || second != `{"outcome":"stop"}` {
			t.Fatalf("responses out of order: %q then %q", first, second)
		}

		calls := f.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2", len(calls))
		}
		if calls[0].Agent != "evaluator" || calls[0].Message != "message one" {
			t.Fatalf("first call = %+v, want the verbatim evaluator invoke", calls[0])
		}
	})

	t.Run("Given a fake whose agent is configured to fail, when the agent is invoked, assert that the configured error is returned", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		want := errors.New("connection refused")
		f.Fail("evaluator", want)

		_, err := f.InvokeAgent(context.Background(), "evaluator", "hello")
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want the configured failure", err)
		}
	})

	t.Run("Given a fake whose agent hangs, when the invoke context expires, assert that the context error is returned", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.Hang("evaluator")

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := f.InvokeAgent(ctx, "evaluator", "hello")
		if err == nil || !strings.Contains(err.Error(), "context") {
			t.Fatalf("err = %v, want the context deadline error", err)
		}
	})

	t.Run("Given a fake without a scripted response for an agent, when that agent is invoked, assert that the invoke fails loudly instead of inventing a response", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()

		_, err := f.InvokeAgent(context.Background(), "evaluator", "hello")
		if err == nil || !strings.Contains(err.Error(), "no scripted response") {
			t.Fatalf("err = %v, want the unscripted-agent error", err)
		}
	})

	t.Run("Given a fake, when CallCount is read, assert that it counts only the named agent's invocations", func(t *testing.T) {
		f := a2a.NewFake()
		defer f.Close()
		f.Script("evaluator", "ok")
		f.Script("worker", "ok")

		_, _ = f.InvokeAgent(context.Background(), "evaluator", "a")
		_, _ = f.InvokeAgent(context.Background(), "worker", "b")

		if f.CallCount("evaluator") != 1 || f.CallCount("worker") != 1 {
			t.Fatalf("call counts = evaluator %d, worker %d; want 1 and 1",
				f.CallCount("evaluator"), f.CallCount("worker"))
		}
	})
}
