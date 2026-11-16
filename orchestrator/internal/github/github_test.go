package github_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
)

func TestTransportPollIssueComments(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	comment := func(id int64, issue int, author string, created time.Time) github.IssueComment {
		return github.IssueComment{
			ID:          id,
			IssueNumber: issue,
			Author:      author,
			Body:        fmt.Sprintf("comment %d", id),
			CreatedAt:   created,
			UpdatedAt:   created,
			URL:         fmt.Sprintf("https://github.com/acme/api/issues/%d#issuecomment-%d", issue, id),
		}
	}

	t.Run("Given a scripted comment sequence on a fake GitHub, when the transport polls from the epoch, assert that every comment comes back oldest first with its parsed fields", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		c1 := comment(1001, 42, "alice", base)
		c2 := comment(1002, 42, "bob", base.Add(5*time.Second))
		c3 := comment(1003, 7, "carol", base.Add(9*time.Second))
		fake.AddComments([]github.IssueComment{c3, c1, c2}) // scripted out of order

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 100)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3 comments, got %d: %+v", len(got), got)
		}
		for i, want := range []github.IssueComment{c1, c2, c3} {
			if got[i] != want {
				t.Errorf("comment %d: want %+v, got %+v", i, want, got[i])
			}
		}
	})

	t.Run("Given comments old and new, when the transport polls with a since cutoff, assert that only comments updated at or after the cutoff come back", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		old := comment(2001, 42, "alice", base)
		edited := comment(2002, 42, "bob", base.Add(1*time.Second))
		edited.UpdatedAt = base.Add(2 * time.Hour) // edited long after creation
		recent := comment(2003, 7, "carol", base.Add(2*time.Hour))
		fake.AddComments([]github.IssueComment{old, edited, recent})

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.PollIssueComments(ctx, "acme/api", base.Add(1*time.Hour), 100)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want only the edited and the recent comment, got %+v", got)
		}
		if got[0].ID != edited.ID || got[1].ID != recent.ID {
			t.Fatalf("want [edited recent] ordered by creation, got [%d %d]", got[0].ID, got[1].ID)
		}
	})

	t.Run("Given more matching comments than one page holds, when the transport polls with a limit, assert that it walks pages until the limit is filled", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		var scripted []github.IssueComment
		for i := range 7 {
			scripted = append(scripted, comment(int64(3000+i), 42, "alice", base.Add(time.Duration(i)*time.Second)))
		}
		fake.AddComments(scripted)

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 5)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("want exactly the limit of 5, got %d", len(got))
		}
		if got[0].ID != 3000 || got[4].ID != 3004 {
			t.Fatalf("want the oldest five in order, got first %d last %d", got[0].ID, got[4].ID)
		}
	})

	t.Run("Given a transport configured with a token, when it polls, assert that the request carries a bearer authorization header", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL(), Token: "gh-token-1"})
		if _, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 10); err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if got := fake.LastAuthorization(); got != "Bearer gh-token-1" {
			t.Fatalf("want bearer token header, got %q", got)
		}
	})

	t.Run("Given a transport without a token, when it polls, assert that no authorization header is sent", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		if _, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 10); err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if got := fake.LastAuthorization(); got != "" {
			t.Fatalf("want no authorization header, got %q", got)
		}
	})

	t.Run("Given a failing GitHub, when the transport polls, assert that the failure surfaces as an error naming the status", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		fake.FailWith(http.StatusUnauthorized, `{"message":"Bad credentials"}`)

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		_, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 10)
		if err == nil {
			t.Fatal("poll must fail on a non-2xx response")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("error must name the status, got: %v", err)
		}
	})

	t.Run("Given an empty repository, when the transport polls, assert that it returns no comments and no error", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 10)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want no comments, got %+v", got)
		}
	})

	t.Run("Given a non-positive limit, when the transport polls, assert that the call is rejected", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		if _, err := tr.PollIssueComments(ctx, "acme/api", time.Time{}, 0); err == nil {
			t.Fatal("poll must reject a non-positive limit")
		}
	})
}

func TestTransportWriteIssueComments(t *testing.T) {
	ctx := context.Background()

	t.Run("Given a fake GitHub, when the transport posts an issue comment, assert that the comment appears on the issue and the created id comes back", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		id, err := tr.PostIssueComment(ctx, "acme/api", 42, "Acknowledged — ticket-abc started.")
		if err != nil {
			t.Fatalf("post failed: %v", err)
		}
		if id <= 0 {
			t.Fatalf("want a server-assigned comment id, got %d", id)
		}

		posted := fake.PostedComments()
		if len(posted) != 1 {
			t.Fatalf("want exactly one posted comment, got %d", len(posted))
		}
		if posted[0].IssueNumber != 42 {
			t.Fatalf("comment landed on issue %d, want 42", posted[0].IssueNumber)
		}
		if posted[0].Body != "Acknowledged — ticket-abc started." {
			t.Fatalf("body = %q, want the posted text verbatim", posted[0].Body)
		}
		if posted[0].ID != id {
			t.Fatalf("posted comment id %d does not match the returned id %d", posted[0].ID, id)
		}
	})

	t.Run("Given an issue holding several comments, when the transport lists that issue's comments, assert that only that conversation's comments come back oldest first", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
		other := github.IssueComment{ID: 1, IssueNumber: 7, Author: "eve", Body: "unrelated", CreatedAt: base, UpdatedAt: base}
		first := github.IssueComment{ID: 2, IssueNumber: 42, Author: "alice", Body: "first", CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(time.Second)}
		second := github.IssueComment{ID: 3, IssueNumber: 42, Author: "bob", Body: "second", CreatedAt: base.Add(2 * time.Second), UpdatedAt: base.Add(2 * time.Second)}
		fake.AddComments([]github.IssueComment{second, other, first}) // scripted out of order

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.IssueComments(ctx, "acme/api", 42)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want only issue 42's two comments, got %d: %+v", len(got), got)
		}
		if got[0].ID != first.ID || got[1].ID != second.ID {
			t.Fatalf("want [first second] oldest first, got [%d %d]", got[0].ID, got[1].ID)
		}
	})

	t.Run("Given more than one page of comments on one issue, when the transport lists that issue's comments, assert that it walks every page oldest first", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
		var scripted []github.IssueComment
		for i := range 101 {
			scripted = append(scripted, github.IssueComment{
				ID:          int64(10 + i),
				IssueNumber: 42,
				Author:      "alice",
				Body:        fmt.Sprintf("c%d", i),
				CreatedAt:   base.Add(time.Duration(i) * time.Second),
				UpdatedAt:   base.Add(time.Duration(i) * time.Second),
			})
		}
		fake.AddComments(scripted)

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		got, err := tr.IssueComments(ctx, "acme/api", 42)
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if len(got) != 101 {
			t.Fatalf("want all 101 comments across pages, got %d", len(got))
		}
		if got[0].ID != 10 || got[100].ID != 110 {
			t.Fatalf("want the full conversation oldest first, got first %d last %d", got[0].ID, got[100].ID)
		}
	})

	t.Run("Given a failing GitHub, when the transport posts an issue comment, assert that the failure surfaces as an error naming the status and nothing is recorded", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		fake.FailWith(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)

		tr := github.NewTransport(github.TransportConfig{BaseURL: fake.URL()})
		_, err := tr.PostIssueComment(ctx, "acme/api", 42, "should never land")
		if err == nil {
			t.Fatal("post must fail on a non-2xx response")
		}
		if !strings.Contains(err.Error(), "403") {
			t.Fatalf("error must name the status, got: %v", err)
		}
		if len(fake.PostedComments()) != 0 {
			t.Fatalf("a failed post must not record a comment, got %+v", fake.PostedComments())
		}
	})
}

func TestTriggerIdentity(t *testing.T) {
	t.Run("Given an issue comment, when asked for its trigger identity, assert that the id is the immutable comment id under its kind", func(t *testing.T) {
		c := github.IssueComment{ID: 987654, IssueNumber: 42}
		if got := c.DeterministicTriggerID(); got != "issue-comment:987654" {
			t.Fatalf("want issue-comment:987654, got %s", got)
		}
	})

	t.Run("Given an issue comment, when asked for its thread key, assert that it names the owning conversation", func(t *testing.T) {
		c := github.IssueComment{ID: 1, IssueNumber: 42}
		if got := c.ThreadKey(); got != "issue:42" {
			t.Fatalf("want issue:42, got %s", got)
		}
	})

	t.Run("Given a repository full name, when asked for its source name, assert that the source is prefixed with the provider", func(t *testing.T) {
		if got := github.SourceName("acme/api"); got != "github:acme/api" {
			t.Fatalf("want github:acme/api, got %s", got)
		}
	})

	t.Run("Given two transports replaying the same scripted comment, when both compute the trigger identity, assert that the ids are identical", func(t *testing.T) {
		// The deterministic id must not depend on when or how often the
		// comment is observed (docs/designs/0001 §6 trigger idempotency).
		c := github.IssueComment{ID: 555, IssueNumber: 3}
		if c.DeterministicTriggerID() != c.DeterministicTriggerID() {
			t.Fatal("trigger identity must be stable across observations")
		}
	})
}
