package outbox_test

// The in-process rate-limit deferral of the drain (PR #11 review, Required
// #4): a GitHub rate limit with a reset deadline holds the affected effect
// out of the drain until the window closes instead of re-hitting the API
// every 2s beat. The hold lives in memory — a restart resets it and the
// first drain re-learns the deadline from the API's Retry-After headers
// (the accepted scope decision).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// fakeOutboxStore is the in-memory outbox.Store: pending effects with
// receipts.
type fakeOutboxStore struct {
	pending   []store.Effect
	delivered []int64
}

func (f *fakeOutboxStore) PendingEffects(_ context.Context) ([]store.Effect, error) {
	return append([]store.Effect(nil), f.pending...), nil
}

func (f *fakeOutboxStore) MarkEffectDelivered(_ context.Context, id int64, _ []byte) error {
	for i, e := range f.pending {
		if e.ID == id {
			f.delivered = append(f.delivered, id)
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			return nil
		}
	}
	return store.ErrEffectNotPending
}

// rateLimitGitHub is the GitHub seam whose comment reads come back clean and
// whose first scripted posts fail with the given errors (FIFO), succeeding
// afterwards.
type rateLimitGitHub struct {
	postErrs []error
	posts    int
}

func (g *rateLimitGitHub) PollIssueComments(_ context.Context, _ string, _ time.Time, _ int) ([]github.IssueComment, error) {
	return nil, nil
}

func (g *rateLimitGitHub) IssueComments(_ context.Context, _ string, _ int) ([]github.IssueComment, error) {
	return nil, nil // no marker in the conversation: the drain must post
}

func (g *rateLimitGitHub) PostIssueComment(_ context.Context, _ string, _ int, _ string) (int64, error) {
	g.posts++
	if len(g.postErrs) > 0 {
		err := g.postErrs[0]
		g.postErrs = g.postErrs[1:]
		if err != nil {
			return 0, err
		}
	}
	return int64(g.posts), nil
}

func (g *rateLimitGitHub) CreatePullRequest(_ context.Context, _ string, _ github.PullRequestInput) (github.PullRequest, error) {
	return github.PullRequest{}, nil
}

func rateLimitEffect(id int64) store.Effect {
	payload, _ := json.Marshal(github.IssueCommentEffect{
		Kind:  github.EffectKindIssueComment,
		Repo:  "acme/api",
		Issue: 42,
		Body:  "the confirmation" + github.EffectMarker("confirm:t1"),
	})
	return store.Effect{
		ID:             id,
		Channel:        github.ChannelGitHub,
		Address:        "acme/api#42",
		IdempotencyKey: "confirm:t1",
		Payload:        payload,
		State:          store.EffectPending,
	}
}

func TestDeliverPendingRateLimitHold(t *testing.T) {
	t.Run("Given a pending effect and a GitHub rate limit with a reset deadline, when the drain runs twice back to back, assert that the first drain holds the effect and the second drain does not re-hit the API (PR #11 review, Required #4)", func(t *testing.T) {
		st := &fakeOutboxStore{pending: []store.Effect{rateLimitEffect(1)}}
		gh := &rateLimitGitHub{postErrs: []error{
			&github.RateLimitError{
				StatusCode: 403,
				RetryAfter: time.Now().Add(200 * time.Millisecond),
				Endpoint:   "https://api.github.com/repos/acme/api/issues/42/comments",
			},
		}}
		d := outbox.New(st, gh)

		stats, err := d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("the rate limit must not fail the drain: %v", err)
		}
		if stats.Deferred != 1 || stats.Delivered != 0 {
			t.Fatalf("stats = %+v, want the effect held behind the backoff", stats)
		}
		if gh.posts != 1 {
			t.Fatalf("posts = %d, want exactly the rate-limited attempt", gh.posts)
		}

		// The immediate re-drain holds the effect out: no second API hit
		// inside the Retry-After window.
		stats, err = d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("held re-drain failed: %v", err)
		}
		if stats.Deferred != 1 || gh.posts != 1 {
			t.Fatalf("stats = %+v posts = %d, want the effect held without a re-hit", stats, gh.posts)
		}
	})

	t.Run("Given an effect held behind an expired backoff, when the drain runs after the window closes, assert that the effect is retried and delivered exactly once", func(t *testing.T) {
		st := &fakeOutboxStore{pending: []store.Effect{rateLimitEffect(1)}}
		gh := &rateLimitGitHub{postErrs: []error{
			&github.RateLimitError{
				StatusCode: 429,
				RetryAfter: time.Now().Add(30 * time.Millisecond),
				Endpoint:   "https://api.github.com/repos/acme/api/issues/42/comments",
			},
		}}
		d := outbox.New(st, gh)

		if _, err := d.DeliverPending(context.Background()); err != nil {
			t.Fatalf("first drain failed: %v", err)
		}
		// The window closes while the beat sleeps.
		time.Sleep(80 * time.Millisecond)
		stats, err := d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("post-window drain failed: %v", err)
		}
		if stats.Delivered != 1 || stats.Deferred != 0 {
			t.Fatalf("stats = %+v, want the effect delivered once the window closed", stats)
		}
		if gh.posts != 2 {
			t.Fatalf("posts = %d, want the retry plus the success", gh.posts)
		}
	})

	t.Run("Given a rate limit without a usable reset deadline, when the drain runs, assert that the failure surfaces as a drain error and the effect stays pending (nothing to defer against)", func(t *testing.T) {
		st := &fakeOutboxStore{pending: []store.Effect{rateLimitEffect(1)}}
		gh := &rateLimitGitHub{postErrs: []error{
			&github.RateLimitError{StatusCode: 403},
		}}
		d := outbox.New(st, gh)

		stats, err := d.DeliverPending(context.Background())
		if err == nil {
			t.Fatal("an unclassifiable rate limit must surface as an error (it stays pending and loud)")
		}
		if stats.Deferred != 0 {
			t.Fatalf("stats = %+v, want nothing deferred without a deadline", stats)
		}
		var rl *github.RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("err = %v, want the scripted rate limit in the chain", err)
		}
		if len(st.pending) != 1 {
			t.Fatalf("pending effects = %d, want the effect kept pending", len(st.pending))
		}
	})
}
