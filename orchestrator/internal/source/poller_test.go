package source_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/source"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
)

const testRepo = "acme/api"

func TestPollOnce(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	// minutes marks a comment i minutes after base.
	minutes := func(i int64) time.Time { return base.Add(time.Duration(i) * time.Minute) }
	// seconds marks a comment i seconds after base: dense enough that the
	// 5s overlap window spans several comments.
	seconds := func(i int64) time.Time { return base.Add(time.Duration(i) * time.Second) }
	comment := func(id int64, issue int, at time.Time) github.IssueComment {
		return github.IssueComment{
			ID:          id,
			IssueNumber: issue,
			Author:      "alice",
			Body:        "@agent please fix",
			CreatedAt:   at,
			UpdatedAt:   at,
			URL:         "https://github.com/acme/api/issues/42#issuecomment-x",
		}
	}

	t.Run("Given a scripted comment sequence on a fake GitHub and a fresh store, when one poll runs, assert that every comment lands as a trigger and the cursor is persisted at the newest occurrence", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		fake.AddComments([]github.IssueComment{
			comment(1001, 42, minutes(1)),
			comment(1002, 42, minutes(3)),
			comment(1003, 7, minutes(9)),
		})
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		res, err := p.PollOnce(ctx)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if res.Fetched != 3 || res.Ingested != 3 || res.Duplicates != 0 || res.Dropped != 0 {
			t.Fatalf("want fetched 3, ingested 3, got %+v", res)
		}
		if res.Source != github.SourceName(testRepo) {
			t.Fatalf("want source %s, got %s", github.SourceName(testRepo), res.Source)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 3 {
			t.Fatalf("want 3 trigger rows, got %d", len(triggers))
		}

		cursor, ok, err := s.SourceCursor(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !ok || !cursor.Equal(minutes(9)) {
			t.Fatalf("want cursor at newest occurrence %s, got %s (ok=%v)", minutes(9), cursor, ok)
		}
	})

	t.Run("Given a completed poll, when the next poll rewinds the cursor by the overlap window, assert that the rewound comments are absorbed as duplicates and no trigger rows are added", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		for i := int64(0); i <= 10; i++ {
			fake.AddComment(comment(2000+i, 42, seconds(i)))
		}
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		if _, err := p.PollOnce(ctx); err != nil {
			t.Fatalf("first poll failed: %v", err)
		}

		res, err := p.PollOnce(ctx)
		if err != nil {
			t.Fatalf("overlap poll failed: %v", err)
		}
		if wantSince := seconds(10).Add(-source.DefaultOverlap); !res.Since.Equal(wantSince) {
			t.Fatalf("poll must rewind the cursor by the overlap: want since %s, got %s", wantSince, res.Since)
		}
		if res.Ingested != 0 || res.Duplicates != 6 {
			t.Fatalf("the overlap window must be absorbed by dedup, got %+v", res)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 11 {
			t.Fatalf("overlap must not add trigger rows, got %d", len(triggers))
		}
	})

	t.Run("Given a completed poll and a new comment arriving afterwards, when the next poll rewinds, assert that only the new comment ingests and the cursor moves to it", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		for i := int64(0); i <= 10; i++ {
			fake.AddComment(comment(2000+i, 42, seconds(i)))
		}
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		if _, err := p.PollOnce(ctx); err != nil {
			t.Fatalf("first poll failed: %v", err)
		}

		fake.AddComment(comment(2100, 42, seconds(13)))
		res, err := p.PollOnce(ctx)
		if err != nil {
			t.Fatalf("second poll failed: %v", err)
		}
		if res.Ingested != 1 || res.Duplicates != 6 {
			t.Fatalf("only the new comment may ingest inside the rewound window, got %+v", res)
		}

		cursor, _, err := s.SourceCursor(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !cursor.Equal(seconds(13)) {
			t.Fatalf("want cursor %s, got %s", seconds(13), cursor)
		}
	})

	t.Run("Given a completed poll and a discarded poller, when a fresh poller polls the same store (restart), assert that it resumes from the persisted cursor instead of re-reading from the origin", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		for i := int64(0); i <= 10; i++ {
			fake.AddComment(comment(2000+i, 42, minutes(i)))
		}
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		if _, err := p.PollOnce(ctx); err != nil {
			t.Fatalf("first poll failed: %v", err)
		}

		// Restart: brand-new transport, intake and poller over the same store.
		restarted, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build restarted poller: %v", err)
		}
		res, err := restarted.PollOnce(ctx)
		if err != nil {
			t.Fatalf("restarted poll failed: %v", err)
		}
		if wantSince := minutes(10).Add(-source.DefaultOverlap); !res.Since.Equal(wantSince) {
			t.Fatalf("a restarted poller must resume from the persisted cursor: want since %s, got %s", wantSince, res.Since)
		}
		if res.Ingested != 0 {
			t.Fatalf("a restarted poller must not re-ingest, got %+v", res)
		}

		triggers, err := s.Triggers(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 11 {
			t.Fatalf("restart must not duplicate triggers, got %d rows", len(triggers))
		}
	})

	t.Run("Given a batch smaller than the backlog, when polls run repeatedly, assert that each poll walks one batch forward until the backlog is consumed exactly once", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		for i := int64(0); i < 10; i++ {
			fake.AddComment(comment(3000+i, 42, minutes(i)))
		}
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s),
			source.Source{Repo: testRepo, BatchSize: 4})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}

		total := 0
		for range 4 {
			res, err := p.PollOnce(ctx)
			if err != nil {
				t.Fatalf("poll failed: %v", err)
			}
			total += res.Ingested
		}
		if total != 10 {
			t.Fatalf("batched polls must ingest every comment exactly once, got %d", total)
		}
	})

	t.Run("Given a configured start cursor and no persisted cursor, when the first poll runs, assert that it reads from the start cursor and skips older comments", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		start := minutes(100)
		fake.AddComment(comment(4001, 42, start.Add(-30*time.Minute))) // before the start cursor
		fake.AddComment(comment(4002, 42, start))                      // exactly at it
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s),
			source.Source{Repo: testRepo, StartCursor: start})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		res, err := p.PollOnce(ctx)
		if err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if !res.Since.Equal(start) {
			t.Fatalf("first poll must read from the start cursor, want %s, got %s", start, res.Since)
		}
		if res.Ingested != 1 {
			t.Fatalf("comments before the start cursor must not ingest, got %+v", res)
		}
	})

	t.Run("Given nothing new on the source, when a poll runs, assert that it ingests nothing and the cursor stays untouched", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		fake.AddComment(comment(5001, 42, minutes(2)))
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: testRepo})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}
		if _, err := p.PollOnce(ctx); err != nil {
			t.Fatalf("first poll failed: %v", err)
		}

		// The rewind re-fetches the last comment (overlap by design); what
		// matters is that no new trigger row appears and the cursor holds.
		res, err := p.PollOnce(ctx)
		if err != nil {
			t.Fatalf("quiet poll failed: %v", err)
		}
		if res.Ingested != 0 || res.Dropped != 0 {
			t.Fatalf("a quiet poll must ingest nothing, got %+v", res)
		}
		cursor, ok, err := s.SourceCursor(ctx, github.SourceName(testRepo))
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !ok || !cursor.Equal(minutes(2)) {
			t.Fatalf("cursor must not move on a quiet poll, got %s", cursor)
		}
	})

	t.Run("Given a source without a repository, when the poller is constructed, assert that construction is refused", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		s := testenv.NewMigratedStore(t)

		if _, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s), source.Source{Repo: ""}); err == nil {
			t.Fatal("a poller without a repository must be refused")
		}
	})
}

func TestRun(t *testing.T) {
	t.Run("Given a running poller, when the context is cancelled, assert that Run returns promptly", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		s := testenv.NewMigratedStore(t)

		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s),
			source.Source{Repo: testRepo, PollInterval: 10 * time.Millisecond})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.Run(ctx, slog.Default())
			close(done)
		}()

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run must return when the context is cancelled")
		}
	})

	t.Run("Given a running poller and a failing GitHub that later recovers, when polls keep ticking, assert that errors do not stop the loop and recovery lands", func(t *testing.T) {
		fake := github.NewFake()
		defer fake.Close()
		s := testenv.NewMigratedStore(t)
		p, err := source.New(github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}), intake.New(s),
			source.Source{Repo: testRepo, PollInterval: 10 * time.Millisecond})
		if err != nil {
			t.Fatalf("failed to build poller: %v", err)
		}

		fake.FailWith(500, `{"message":"boom"}`)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() {
			p.Run(ctx, slog.Default())
			close(done)
		}()

		// The loop keeps ticking through failures.
		deadline := time.Now().Add(1 * time.Second)
		for fake.RequestCount() < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("poller stopped ticking through failures, requests=%d", fake.RequestCount())
			}
			time.Sleep(5 * time.Millisecond)
		}

		now := time.Now().UTC()
		fake.AddComment(github.IssueComment{
			ID: 6001, IssueNumber: 42, Author: "alice", Body: "@agent",
			CreatedAt: now, UpdatedAt: now,
		})
		fake.SetOK()

		// Recovery: the new comment eventually lands in the store.
		deadline = time.Now().Add(1 * time.Second)
		for {
			triggers, err := s.Triggers(context.Background(), github.SourceName(testRepo))
			if err != nil {
				t.Fatalf("read triggers failed: %v", err)
			}
			if len(triggers) == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("recovered polls must ingest, got %d triggers", len(triggers))
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run must return when the context ends")
		}
	})
}
