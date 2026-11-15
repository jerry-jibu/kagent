package dispatch_test

import (
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// Given an oversized push body, when the receiver serves it, assert 413 —
// the unauthenticated listener must not decode unbounded bodies (PR #11
// review, Required).
func TestPushReceiverBodyBound(t *testing.T) {
	st, _, _, _, d := intentFixtureWith(t, func(*store.Store) dispatch.Options {
		return dispatch.Options{WorkerAgent: "kagent/worker"}
	})
	_ = st
	receiver := dispatch.NewReceiver(d, nil)

	huge := `{"task":{"id":"task-big","contextId":"ctx-big","status":{"state":"TASK_STATE_WORKING","message":{"parts":[{"text":"` +
		strings.Repeat("x", 5<<20) + `"}]}}}}`
	rec := postPush(t, receiver, huge)
	if rec.Code != 413 {
		t.Fatalf("oversized push status = %d, want 413 (body: %.100s)", rec.Code, rec.Body.String())
	}
}
