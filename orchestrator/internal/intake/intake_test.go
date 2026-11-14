package intake_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
)

func TestIngestIssueComments(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	const repo = "acme/api"

	comment := func(id int64, issue int, author string, created time.Time) github.IssueComment {
		return github.IssueComment{
			ID:          id,
			IssueNumber: issue,
			Author:      author,
			Body:        "@agent please fix the flaky test",
			CreatedAt:   created,
			UpdatedAt:   created,
			URL:         "https://github.com/acme/api/issues/42#issuecomment-" + itoa(id),
		}
	}

	t.Run("Given a scripted issue-comment sequence, when ingested through Intake, assert that every comment lands as one trigger under the GitHub source with its thread attribution", func(t *testing.T) {
		s := testenv.NewMigratedStore(t)
		in := intake.New(s)

		stats, err := in.IngestIssueComments(ctx, repo, []github.IssueComment{
			comment(1001, 42, "alice", base),
			comment(1002, 42, "bob", base.Add(5*time.Second)),
			comment(1003, 7, "carol", base.Add(9*time.Second)),
		})
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Ingested != 3 || stats.Duplicates != 0 || stats.Dropped != 0 {
			t.Fatalf("want {3 0 0}, got %+v", stats)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(repo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 3 {
			t.Fatalf("want 3 triggers, got %d", len(triggers))
		}
		wantThread := map[string]string{
			"issue-comment:1001": "issue:42",
			"issue-comment:1002": "issue:42",
			"issue-comment:1003": "issue:7",
		}
		for _, tr := range triggers {
			if tr.ThreadKey != wantThread[tr.DeterministicID] {
				t.Errorf("trigger %s: want thread %s, got %s", tr.DeterministicID, wantThread[tr.DeterministicID], tr.ThreadKey)
			}
		}
	})

	t.Run("Given an ingested comment, when its trigger payload is decoded, assert that it carries the full audit record of the comment", func(t *testing.T) {
		s := testenv.NewMigratedStore(t)
		in := intake.New(s)

		c := comment(2001, 42, "alice", base)
		if _, err := in.IngestIssueComments(ctx, repo, []github.IssueComment{c}); err != nil {
			t.Fatalf("ingest failed: %v", err)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(repo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 1 {
			t.Fatalf("want 1 trigger, got %d", len(triggers))
		}
		var payload intake.CommentPayload
		if err := json.Unmarshal(triggers[0].Payload, &payload); err != nil {
			t.Fatalf("trigger payload is not a CommentPayload: %v", err)
		}
		want := intake.CommentPayload{
			Kind:      "issue-comment",
			Repo:      repo,
			Issue:     42,
			CommentID: 2001,
			Author:    "alice",
			Body:      c.Body,
			URL:       c.URL,
			CreatedAt: base,
		}
		if payload != want {
			t.Fatalf("payload mismatch:\n got %+v\nwant %+v", payload, want)
		}
	})

	t.Run("Given the same comment sequence replayed, when ingested again through Intake, assert that nothing new lands and every event reports as duplicate", func(t *testing.T) {
		s := testenv.NewMigratedStore(t)
		in := intake.New(s)

		sequence := []github.IssueComment{
			comment(1001, 42, "alice", base),
			comment(1002, 42, "bob", base.Add(5*time.Second)),
		}
		if _, err := in.IngestIssueComments(ctx, repo, sequence); err != nil {
			t.Fatalf("first ingest failed: %v", err)
		}

		stats, err := in.IngestIssueComments(ctx, repo, sequence)
		if err != nil {
			t.Fatalf("replay failed: %v", err)
		}
		if stats.Ingested != 0 || stats.Duplicates != 2 {
			t.Fatalf("want {0 2 0}, got %+v", stats)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(repo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 2 {
			t.Fatalf("replay must not add rows, got %d", len(triggers))
		}
	})

	t.Run("Given a comment on a thread whose last activity is beyond the default 14-day sightline, when ingested through Intake, assert that it is dropped", func(t *testing.T) {
		s := testenv.NewMigratedStore(t)
		in := intake.New(s)

		stale := time.Now().Add(-15 * 24 * time.Hour).UTC()
		stats, err := in.IngestIssueComments(ctx, repo, []github.IssueComment{
			comment(3001, 9, "alice", stale),
		})
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Dropped != 1 || stats.Ingested != 0 {
			t.Fatalf("a comment beyond the 14-day sightline must be dropped, got %+v", stats)
		}
	})

	t.Run("Given an empty poll batch, when ingested through Intake, assert that it is a no-op", func(t *testing.T) {
		s := testenv.NewMigratedStore(t)
		in := intake.New(s)

		stats, err := in.IngestIssueComments(ctx, repo, nil)
		if err != nil {
			t.Fatalf("empty ingest failed: %v", err)
		}
		if stats != (store.IngestStats{}) {
			t.Fatalf("empty batch must be a no-op, got %+v", stats)
		}
	})
}

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
