package store

// Internal tests for the sightline predicate: asserting thread state and
// live tickets requires raw rows the store's public API does not (yet) write.
// A thread with stale ingested activity is seeded directly — through the
// ingest path such a thread can only arise by real time passing after a
// fresh ingest. Behavior-level coverage of ingestion lives in store_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// testDatabaseURL is the package-internal twin of the external helper.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL test")
	}
	return url
}

// openInternalTestStore is the package-internal twin of openTestStore.
func openInternalTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, Config{
		DatabaseURL: testDatabaseURL(t),
		Schema:      fmt.Sprintf("orch_test_%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() {
		s.DropSchema(context.Background())
		s.Close()
	})
	return s
}

func mustMigrateInternal(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
}

func (s *Store) seedThreadForTest(t *testing.T, source, threadKey string, lastActivity time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO threads (source, thread_key, last_activity_at) VALUES ($1, $2, $3)`,
		source, threadKey, lastActivity); err != nil {
		t.Fatalf("failed to seed thread %s/%s: %v", source, threadKey, err)
	}
}

func (s *Store) insertTicketForTest(t *testing.T, id, threadSource, threadKey, status string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO tickets (id, thread_source, thread_key, mandate, status)
		VALUES ($1, $2, $3, '{"test":true}', $4)`,
		id, threadSource, threadKey, status); err != nil {
		t.Fatalf("failed to insert test ticket %s: %v", id, err)
	}
}

func (s *Store) threadLastActivity(t *testing.T, source, threadKey string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(context.Background(),
		"SELECT last_activity_at FROM threads WHERE source = $1 AND thread_key = $2",
		source, threadKey).Scan(&at); err != nil {
		t.Fatalf("failed to read thread activity for %s/%s: %v", source, threadKey, err)
	}
	return at
}

func TestIngestSightline(t *testing.T) {
	const source = "github:acme/api"
	sightline := 14 * 24 * time.Hour

	// seeded activity 15 days ago: beyond the sightline window
	stale := time.Now().Add(-15 * 24 * time.Hour).UTC()
	// the event a poller would deliver now
	fresh := time.Now().UTC()

	t.Run("Given a thread with no ticket whose last ingested activity is 15 days old, when a new event arrives, assert that the event is dropped and the thread's sightline is not refreshed", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)
		s.seedThreadForTest(t, source, "issue:1", stale)

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			{ThreadKey: "issue:1", DeterministicID: "issue-comment:2", Payload: []byte(`{}`), OccurredAt: fresh},
		}, sightline)
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Dropped != 1 || stats.Ingested != 0 {
			t.Fatalf("want the stale-thread event dropped, got %+v", stats)
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 0 {
			t.Fatalf("no trigger row may be created for a dropped event, got %+v", triggers)
		}
		if got := s.threadLastActivity(t, source, "issue:1"); got.Sub(stale).Abs() > 5*time.Second {
			t.Fatalf("a drop must not refresh the sightline: want ~%s, got %s", stale, got)
		}
	})

	t.Run("Given a thread beyond the sightline that holds a live ticket, when a new event arrives, assert that the event is ingested", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)
		s.seedThreadForTest(t, source, "issue:2", stale)
		s.insertTicketForTest(t, "ticket-1", source, "issue:2", TicketStatusPending)

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			{ThreadKey: "issue:2", DeterministicID: "issue-comment:4", Payload: []byte(`{}`), OccurredAt: fresh},
		}, sightline)
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Ingested != 1 || stats.Dropped != 0 {
			t.Fatalf("a live ticket keeps the thread visible, got %+v", stats)
		}
	})

	t.Run("Given a thread beyond the sightline whose only ticket has ended, when a new event arrives, assert that the event is dropped", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)
		s.seedThreadForTest(t, source, "issue:3", stale)
		s.insertTicketForTest(t, "ticket-2", source, "issue:3", TicketStatusEnded)

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			{ThreadKey: "issue:3", DeterministicID: "issue-comment:6", Payload: []byte(`{}`), OccurredAt: fresh},
		}, sightline)
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Dropped != 1 || stats.Ingested != 0 {
			t.Fatalf("an ended ticket must not keep the thread visible, got %+v", stats)
		}
	})

	t.Run("Given a thread's first-ever event older than the sightline, when it arrives during backlog catch-up, assert that the event is dropped and the thread is recorded with its stale activity", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			{ThreadKey: "issue:9", DeterministicID: "issue-comment:9", Payload: []byte(`{}`), OccurredAt: stale},
		}, sightline)
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Dropped != 1 || stats.Ingested != 0 {
			t.Fatalf("a first event beyond the sightline must be dropped, got %+v", stats)
		}
		if got := s.threadLastActivity(t, source, "issue:9"); got.Sub(stale).Abs() > 5*time.Second {
			t.Fatalf("the thread must carry its own stale activity: want ~%s, got %s", stale, got)
		}
	})

	t.Run("Given a dropped event, when the cursor is read back, assert that it still advanced past the dropped event", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)
		s.seedThreadForTest(t, source, "issue:4", stale)

		if _, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			{ThreadKey: "issue:4", DeterministicID: "issue-comment:8", Payload: []byte(`{}`), OccurredAt: fresh},
		}, sightline); err != nil {
			t.Fatalf("ingest failed: %v", err)
		}

		cursor, ok, err := s.SourceCursor(ctx, source)
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !ok || !cursor.Equal(fresh) {
			t.Fatalf("cursor must advance past dropped events: want %s, got %s (ok=%v)", fresh, cursor, ok)
		}
	})
}

func TestIngestInboxClaim(t *testing.T) {
	const source = "github:acme/api"
	event := func(id string) Event {
		return Event{ThreadKey: "issue:1", DeterministicID: id, Payload: []byte(`{}`), OccurredAt: time.Now().UTC()}
	}

	t.Run("Given a worker already holding the inbox claim, when another worker ingests the same inbox, assert that the claim is refused and nothing persists", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)

		// Simulate the other worker: hold the inbox's advisory claim on an
		// open transaction, the same mechanism IngestEvents uses.
		holder, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin holder transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx,
			"SELECT pg_try_advisory_xact_lock($1)", s.inboxKey(source)); err != nil {
			t.Fatalf("failed to hold inbox claim: %v", err)
		}

		_, err = s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			event("issue-comment:1"),
		}, 14*24*time.Hour)
		if !errors.Is(err, ErrInboxLocked) {
			t.Fatalf("want ErrInboxLocked while another worker holds the inbox, got: %v", err)
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 0 {
			t.Fatalf("a refused claim must persist nothing, got %+v", triggers)
		}
		cursor, ok, err := s.SourceCursor(ctx, source)
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if ok {
			t.Fatalf("a refused claim must not advance the cursor, got %s", cursor)
		}
	})

	t.Run("Given a released inbox claim, when a worker ingests the inbox again, assert that the claim succeeds", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)

		holder, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin holder transaction: %v", err)
		}
		if _, err := holder.Exec(ctx,
			"SELECT pg_try_advisory_xact_lock($1)", s.inboxKey(source)); err != nil {
			t.Fatalf("failed to hold inbox claim: %v", err)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("failed to release inbox claim: %v", err)
		}

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			event("issue-comment:1"),
		}, 14*24*time.Hour)
		if err != nil {
			t.Fatalf("ingest must succeed once the claim is released: %v", err)
		}
		if stats.Ingested != 1 {
			t.Fatalf("want the event ingested after release, got %+v", stats)
		}
	})

	t.Run("Given two different inboxes, when two workers claim concurrently, assert that both proceed independently", func(t *testing.T) {
		s := openInternalTestStore(t)
		ctx := context.Background()
		mustMigrateInternal(t, s)

		holder, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("failed to begin holder transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx,
			"SELECT pg_try_advisory_xact_lock($1)", s.inboxKey("github:other/repo")); err != nil {
			t.Fatalf("failed to hold other inbox claim: %v", err)
		}

		stats, err := s.IngestEvents(ctx, SourceRef{Name: source, Kind: "github"}, []Event{
			event("issue-comment:1"),
		}, 14*24*time.Hour)
		if err != nil {
			t.Fatalf("claims on different inboxes must not contend: %v", err)
		}
		if stats.Ingested != 1 {
			t.Fatalf("want the event ingested, got %+v", stats)
		}
	})
}
