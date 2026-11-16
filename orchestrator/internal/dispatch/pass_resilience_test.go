package dispatch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/a2a"
	"github.com/jerry-jibu/kagent/orchestrator/internal/dispatch"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
)

// failingDeliverer is an outbox drain that always errors — the poisoned
// delivery path.
type failingDeliverer struct{ calls int }

func (f *failingDeliverer) DeliverPending(ctx context.Context) (outbox.DeliverStats, error) {
	f.calls++
	return outbox.DeliverStats{}, errors.New("poisoned effect row")
}

// Given a beat whose outbox drain fails, when Tick returns, assert that the
// error is surfaced AND the stage stats are not lost — and that the drain
// still ran after every earlier stage (PR #11 review, Required: per-item
// and per-stage failures must not starve user-visible deliveries).
func TestPassTickDrainResilience(t *testing.T) {
	ctx := context.Background()
	// A pending assignment with its trigger ingested: the beat's dispatch
	// stage lands one task.
	st, tk, _ := acceptFixture(t)
	storeTriggerPayload(t, st)
	fake := a2a.NewFake()
	fake.ScriptDispatch("kagent/worker", a2a.DispatchResult{TaskID: "task-d1", ContextID: tk.SessionID})

	fd := &failingDeliverer{}
	pass := dispatch.NewPass(st, dispatch.New(st, fake, dispatch.Options{
		WorkerAgent: "kagent/worker",
		PushBaseURL: "http://orchestrator:8080",
	}), fd, dispatch.PassOptions{MaxFollowups: 3})

	stats, err := pass.Tick(ctx)
	if err == nil {
		t.Fatalf("the drain failure must surface as an error")
	}
	if stats.Dispatched != 1 {
		t.Fatalf("stats lost with the error: stats=%+v err=%v", stats, err)
	}
	if fd.calls != 1 {
		t.Fatalf("the drain ran %d times, want exactly once per tick", fd.calls)
	}

	// A second tick still reaches the drain — an earlier stage's failure
	// never starves the next tick's deliveries.
	if _, err := pass.Tick(ctx); err == nil {
		t.Fatalf("second tick keeps surfacing the poisoned drain")
	}
	if fd.calls != 2 {
		t.Fatalf("drain calls = %d, want 2", fd.calls)
	}
}
