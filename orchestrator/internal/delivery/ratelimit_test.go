package delivery_test

// The rate-limit deferral contract (PR #11 review, Required #3): a GitHub
// rate limit with a usable reset deadline must DEFER the ticket — the
// deliverer writes the store's DeferTicketDelivery and the candidate leaves
// the pending-deliveries scan until the block lifts. These tests run on
// fakes only (no PostgreSQL): the store contract is the seam the deliverer
// calls, and the fake's filter mirrors the store-side PendingDeliveries
// exclusion.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/delivery"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// fakeStore is the in-memory delivery.Store: the candidate list with the
// deferral filter the real store implements over rate_limited_until.
type fakeStore struct {
	candidates []store.DeliveryCandidate
	deferred   map[string]time.Time
	deferCalls []deferCall
	audits     []store.AuditEvent
}

type deferCall struct {
	ticketID string
	until    time.Time
}

func newFakeStore(candidates ...store.DeliveryCandidate) *fakeStore {
	return &fakeStore{candidates: candidates, deferred: map[string]time.Time{}}
}

func (f *fakeStore) PendingDeliveries(_ context.Context) ([]store.DeliveryCandidate, error) {
	now := time.Now()
	var out []store.DeliveryCandidate
	for _, c := range f.candidates {
		if until, ok := f.deferred[c.Ticket.ID]; ok && now.Before(until) {
			continue // deferred: the scan does not re-pick the ticket
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) DeferTicketDelivery(_ context.Context, ticketID string, until time.Time) error {
	f.deferCalls = append(f.deferCalls, deferCall{ticketID: ticketID, until: until})
	f.deferred[ticketID] = until
	return nil
}

func (f *fakeStore) RecordAudit(_ context.Context, ticketID string, event string, detail any) error {
	f.audits = append(f.audits, store.AuditEvent{Event: event, Detail: map[string]any{"_ticket": ticketID, "_detail": detail}})
	return nil
}

func (f *fakeStore) AuditEvents(_ context.Context, _ string) ([]store.AuditEvent, error) {
	// The ready report the PR body quotes.
	return []store.AuditEvent{{Event: "report.ready", Detail: map[string]any{
		"summary":   "the rate-limited summary",
		"artifacts": []any{"app.go"},
	}}}, nil
}

func (f *fakeStore) FailTicketDelivery(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func (f *fakeStore) MarkTicketDelivered(_ context.Context, _ store.DeliveredPlan) (bool, error) {
	return true, nil
}

// rateLimitTransport is the GitHub seam whose CreatePullRequest answers the
// scripted error — the rate limit the delivery tail hits.
type rateLimitTransport struct {
	err     error
	prCalls int
}

func (t *rateLimitTransport) PollIssueComments(_ context.Context, _ string, _ time.Time, _ int) ([]github.IssueComment, error) {
	return nil, nil
}

func (t *rateLimitTransport) IssueComments(_ context.Context, _ string, _ int) ([]github.IssueComment, error) {
	return nil, nil
}

func (t *rateLimitTransport) PostIssueComment(_ context.Context, _ string, _ int, _ string) (int64, error) {
	return 1, nil
}

func (t *rateLimitTransport) CreatePullRequest(_ context.Context, _ string, _ github.PullRequestInput) (github.PullRequest, error) {
	t.prCalls++
	if t.err != nil {
		return github.PullRequest{}, t.err
	}
	return github.PullRequest{Number: 7, URL: "https://github.com/acme/api/pull/7"}, nil
}

// rateLimitFixture wires the deliverer on the fake store (holding one ready
// candidate for ticketID), the in-memory workspace backend and a live bare
// git remote — everything except PostgreSQL (the PG-backed fixture skips
// without DATABASE_URL; the deferral contract is unit-tested at the seam).
// Returns the store, the transport and the bare origin so tests can assert
// on the deferral writes and the pushed branch.
func rateLimitFixture(t *testing.T, ticketID string, gh *rateLimitTransport) (*delivery.Deliverer, *fakeStore, *rateLimitTransport, string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	origin := filepath.Join(root, "acme", "api.git")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	runGit(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "seed")
	baseSHA := runGit(t, seed, "rev-parse", "HEAD")
	runGit(t, root, "clone", "--bare", "--no-hardlinks", seed, origin)
	runGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	backend := newMemBackend()
	if err := backend.EnsureWorkspace(context.Background(), ticketID); err != nil {
		t.Fatalf("EnsureWorkspace failed: %v", err)
	}
	if err := backend.WriteFile(context.Background(), ticketID, "app.go", []byte("package app\n\n// rewritten\n")); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	st := newFakeStore(rateLimitedCandidate(ticketID, baseSHA))
	return delivery.New(st, backend, gh, delivery.Options{GitBaseURL: "file://" + root}), st, gh, origin
}

// rateLimitedCandidate is one ready delivery candidate for acme/api#42,
// materialized from baseSHA.
func rateLimitedCandidate(ticketID, baseSHA string) store.DeliveryCandidate {
	return store.DeliveryCandidate{
		Ticket: store.Ticket{
			ID:     ticketID,
			Status: store.TicketStatusEnded,
			Mandate: store.Mandate{
				PolicyVersion: "abc123",
				Strength:      "explicit",
				Sender:        "alice",
				Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
			},
		},
		BaseBranch: "main",
		BaseCommit: baseSHA,
	}
}

// gitBranchExists reports whether the branch exists on the bare origin.
func gitBranchExists(t *testing.T, origin, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", origin, "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func TestDeliverPendingRateLimitDeferral(t *testing.T) {
	t.Run("Given a ready candidate and a GitHub rate limit carrying a reset deadline, when the delivery beat runs and runs again, assert that DeferTicketDelivery is written with the RetryAfter value, the candidate leaves the scan, and no further PR attempt happens while deferred (PR #11 review, Required #3)", func(t *testing.T) {
		retryAfter := time.Now().Add(30 * time.Minute).Round(time.Second)
		gh := &rateLimitTransport{err: &github.RateLimitError{
			StatusCode: 403,
			RetryAfter: retryAfter,
			Endpoint:   "https://api.github.com/repos/acme/api/pulls",
		}}
		d, st, _, origin := rateLimitFixture(t, "ticket-rl", gh)

		stats, err := d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.RateLimited != 1 {
			t.Fatalf("stats = %+v, want one rate-limited candidate", stats)
		}
		if len(st.deferCalls) != 1 || st.deferCalls[0].ticketID != "ticket-rl" || !st.deferCalls[0].until.Equal(retryAfter) {
			t.Fatalf("deferral writes = %+v, want the ticket deferred with the RetryAfter value %s", st.deferCalls, retryAfter.Format(time.RFC3339))
		}
		// The branch push survived before the rate limit hit: the retry
		// stays replay-safe.
		if !gitBranchExists(t, origin, "ticket-rl") {
			t.Fatalf("the ticket branch must persist for the post-deferral retry")
		}
		if calls := gh.prCalls; calls != 1 {
			t.Fatalf("PR calls = %d, want exactly the rate-limited attempt", calls)
		}

		// The next beat: the deferral filter holds the candidate out of the
		// scan — nothing ships, nothing re-hits GitHub.
		stats, err = d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("deferred DeliverPending failed: %v", err)
		}
		if stats.Delivered+stats.Confirmed+stats.Failed+stats.Retried+stats.RateLimited != 0 {
			t.Fatalf("deferred beat stats = %+v, want an empty scan", stats)
		}
		if calls := gh.prCalls; calls != 1 {
			t.Fatalf("PR calls = %d after the deferred beat, want no re-pick while the deferral holds", calls)
		}
	})

	t.Run("Given a rate limit without a usable reset deadline, when the delivery beat runs, assert that no deferral is written and the candidate stays pending for the next beat (an unclassified 403 retries with care)", func(t *testing.T) {
		gh := &rateLimitTransport{err: &github.RateLimitError{
			StatusCode: 403,
			Endpoint:   "https://api.github.com/repos/acme/api/pulls",
		}}
		d, st, _, _ := rateLimitFixture(t, "ticket-rl", gh)

		stats, err := d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.RateLimited != 1 {
			t.Fatalf("stats = %+v, want one rate-limited candidate", stats)
		}
		if len(st.deferCalls) != 0 {
			t.Fatalf("deferral writes = %+v, want none without a usable deadline", st.deferCalls)
		}

		// The candidate did not leave the scan: the next beat retries it.
		stats, err = d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("retried DeliverPending failed: %v", err)
		}
		if stats.RateLimited != 1 || gh.prCalls != 2 {
			t.Fatalf("stats = %+v PR calls = %d, want the candidate re-attempted on the next beat", stats, gh.prCalls)
		}
	})
}
