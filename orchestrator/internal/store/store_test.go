package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// testDatabaseURL returns the DATABASE_URL value or skips the test.
// PG tests run against a real PostgreSQL; see CONTEXT.md for the local setup.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL test")
	}
	return url
}

// openTestStore opens a Store against a fresh, uniquely named schema so tests
// never observe each other's state.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, store.Config{
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

func mustMigrate(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
}

func TestIngestEvents(t *testing.T) {
	// A scripted issue-comment sequence: two comments on one issue and one
	// on another, in ascending occurrence order.
	sequence := []store.Event{
		{
			ThreadKey:       "issue:42",
			DeterministicID: "issue-comment:1001",
			Payload:         []byte(`{"kind":"issue-comment","comment_id":1001}`),
			OccurredAt:      time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		},
		{
			ThreadKey:       "issue:42",
			DeterministicID: "issue-comment:1002",
			Payload:         []byte(`{"kind":"issue-comment","comment_id":1002}`),
			OccurredAt:      time.Date(2026, 11, 14, 10, 0, 5, 0, time.UTC),
		},
		{
			ThreadKey:       "issue:7",
			DeterministicID: "issue-comment:1003",
			Payload:         []byte(`{"kind":"issue-comment","comment_id":1003}`),
			OccurredAt:      time.Date(2026, 11, 14, 10, 0, 9, 0, time.UTC),
		},
	}
	const source = "github:acme/api"

	t.Run("Given a scripted issue-comment sequence, when ingested into a fresh store, assert that every event lands as exactly one trigger on its thread with the persisted payload", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		stats, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour)
		if err != nil {
			t.Fatalf("ingest failed: %v", err)
		}
		if stats.Ingested != 3 || stats.Duplicates != 0 || stats.Dropped != 0 {
			t.Fatalf("want {3 0 0}, got %+v", stats)
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 3 {
			t.Fatalf("want 3 trigger rows, got %d: %+v", len(triggers), triggers)
		}
		wantThreads := map[string]string{
			"issue-comment:1001": "issue:42",
			"issue-comment:1002": "issue:42",
			"issue-comment:1003": "issue:7",
		}
		for _, tr := range triggers {
			if tr.Source != source {
				t.Errorf("trigger %s: want source %s, got %s", tr.DeterministicID, source, tr.Source)
			}
			if wantThreads[tr.DeterministicID] != tr.ThreadKey {
				t.Errorf("trigger %s: want thread %s, got %s", tr.DeterministicID, wantThreads[tr.DeterministicID], tr.ThreadKey)
			}
		}
		byID := map[string]store.Trigger{}
		for _, tr := range triggers {
			byID[tr.DeterministicID] = tr
		}
		// Payload is stored as JSONB, which normalizes whitespace and key
		// order: equality is semantic, not byte-for-byte.
		var gotPayload, wantPayload map[string]any
		if err := json.Unmarshal(byID["issue-comment:1001"].Payload, &gotPayload); err != nil {
			t.Fatalf("stored payload is not JSON: %v", err)
		}
		if err := json.Unmarshal(sequence[0].Payload, &wantPayload); err != nil {
			t.Fatalf("scripted payload is not JSON: %v", err)
		}
		if !reflect.DeepEqual(gotPayload, wantPayload) {
			t.Errorf("trigger payload not persisted: got %s, want %s", byID["issue-comment:1001"].Payload, sequence[0].Payload)
		}
	})

	t.Run("Given an already-ingested sequence, when the exact same sequence is replayed, assert that no new trigger rows appear and every event reports as duplicate", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		if _, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour); err != nil {
			t.Fatalf("first ingest failed: %v", err)
		}

		stats, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour)
		if err != nil {
			t.Fatalf("replay ingest failed: %v", err)
		}
		if stats.Ingested != 0 || stats.Duplicates != 3 || stats.Dropped != 0 {
			t.Fatalf("want {0 3 0}, got %+v", stats)
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 3 {
			t.Fatalf("replay must not add rows: want 3 trigger rows, got %d", len(triggers))
		}
	})

	t.Run("Given a batch with an event missing its deterministic identity, when ingested, assert that the whole batch is rejected and nothing persists", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		bad := []store.Event{{
			ThreadKey:       "issue:42",
			DeterministicID: "",
			Payload:         []byte(`{}`),
			OccurredAt:      time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		}}
		if _, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, bad, 14*24*time.Hour); err == nil {
			t.Fatal("ingest must reject an event without a deterministic id")
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != 0 {
			t.Fatalf("nothing may persist from a rejected batch, got %d rows", len(triggers))
		}
	})

	t.Run("Given an ingested batch, when the persisted cursor is read back, assert that it equals the newest occurrence in the batch", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		if _, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour); err != nil {
			t.Fatalf("ingest failed: %v", err)
		}

		cursor, ok, err := s.SourceCursor(ctx, source)
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !ok {
			t.Fatal("cursor must be persisted after ingesting a batch")
		}
		want := sequence[2].OccurredAt
		if !cursor.Equal(want) {
			t.Fatalf("want cursor %s, got %s", want, cursor)
		}
	})

	t.Run("Given an older batch followed by a newer one, when both are ingested, assert that the cursor only moves forward", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		if _, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour); err != nil {
			t.Fatalf("first ingest failed: %v", err)
		}
		overlap := []store.Event{{
			ThreadKey:       "issue:42",
			DeterministicID: "issue-comment:1002", // duplicate inside the rewind window
			Payload:         sequence[1].Payload,
			OccurredAt:      sequence[1].OccurredAt,
		}}
		if _, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, overlap, 14*24*time.Hour); err != nil {
			t.Fatalf("overlap ingest failed: %v", err)
		}

		cursor, _, err := s.SourceCursor(ctx, source)
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if !cursor.Equal(sequence[2].OccurredAt) {
			t.Fatalf("cursor must not regress: want %s, got %s", sequence[2].OccurredAt, cursor)
		}
	})

	t.Run("Given a source never ingested, when its cursor is read, assert that it reports absent", func(t *testing.T) {
		s := openTestStore(t)
		mustMigrate(t, s)

		_, ok, err := s.SourceCursor(context.Background(), source)
		if err != nil {
			t.Fatalf("read cursor failed: %v", err)
		}
		if ok {
			t.Fatal("cursor must be absent before the first ingest")
		}
	})

	t.Run("Given many workers ingesting the same inbox concurrently, when they all run, assert that every trigger exists exactly once and only claim refusals are reported as errors", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		mustMigrate(t, s)

		const workers = 8
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // maximize genuine overlap
				_, err := s.IngestEvents(ctx, store.SourceRef{Name: source, Kind: "github"}, sequence, 14*24*time.Hour)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			if err != nil && !errors.Is(err, store.ErrInboxLocked) {
				t.Fatalf("only ErrInboxLocked may be reported under contention, got: %v", err)
			}
		}

		triggers, err := s.Triggers(ctx, source)
		if err != nil {
			t.Fatalf("read triggers failed: %v", err)
		}
		if len(triggers) != len(sequence) {
			t.Fatalf("dedup must keep exactly one row per event, want %d rows, got %d", len(sequence), len(triggers))
		}
		seen := map[string]bool{}
		for _, tr := range triggers {
			if seen[tr.DeterministicID] {
				t.Fatalf("duplicate trigger row for %s", tr.DeterministicID)
			}
			seen[tr.DeterministicID] = true
		}
	})
}

func TestMigrate(t *testing.T) {
	t.Run("Given a fresh empty schema, when migrations are applied, assert that all orchestrator tables exist and the migration is recorded", func(t *testing.T) {
		s := openTestStore(t)

		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		tables, err := s.ListTables(context.Background())
		if err != nil {
			t.Fatalf("list tables failed: %v", err)
		}
		want := []string{
			"assignments",
			"audit",
			"decisions",
			"outbox",
			"policy",
			"schema_migrations",
			"sources",
			"threads",
			"tickets",
			"timers",
			"triggers",
		}
		if len(tables) != len(want) {
			t.Fatalf("expected exactly %v tables, got %v", want, tables)
		}
		for i, name := range want {
			if tables[i] != name {
				t.Fatalf("table %d: want %q, got %q", i, name, tables[i])
			}
		}

		versions, err := s.AppliedMigrations(context.Background())
		if err != nil {
			t.Fatalf("applied migrations failed: %v", err)
		}
		wantVersions := []string{"0001_init", "0002_audit", "0002_intake", "0003_decisions"}
		if !reflect.DeepEqual(versions, wantVersions) {
			t.Fatalf("expected %v recorded, got %v", wantVersions, versions)
		}
	})

	t.Run("Given an already-migrated schema, when migrations are applied again, assert that the run is a no-op and still succeeds", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("first migrate failed: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second migrate failed: %v", err)
		}

		versions, err := s.AppliedMigrations(ctx)
		if err != nil {
			t.Fatalf("applied migrations failed: %v", err)
		}
		wantVersions := []string{"0001_init", "0002_audit", "0002_intake", "0003_decisions"}
		if !reflect.DeepEqual(versions, wantVersions) {
			t.Fatalf("re-run must not duplicate migrations: expected %v, got %v", wantVersions, versions)
		}
	})
}

func TestTryAcquireInstanceLock(t *testing.T) {
	t.Run("Given an unlocked database, when a store acquires the instance lock, assert that the lock is granted", func(t *testing.T) {
		s := openTestStore(t)

		err := s.TryAcquireInstanceLock(context.Background())
		if err != nil {
			t.Fatalf("lock acquisition should succeed on unlocked database: %v", err)
		}
	})

	t.Run("Given an instance lock already held by another store, when a second store tries to acquire it, assert that acquisition is refused", func(t *testing.T) {
		first := openTestStore(t)
		if err := first.TryAcquireInstanceLock(context.Background()); err != nil {
			t.Fatalf("first lock acquisition failed: %v", err)
		}

		second, err := store.Open(context.Background(), store.Config{
			DatabaseURL: testDatabaseURL(t),
			Schema:      first.Schema(),
		})
		if err != nil {
			t.Fatalf("failed to open second store: %v", err)
		}
		defer func() {
			second.Close()
		}()

		err = second.TryAcquireInstanceLock(context.Background())
		if err == nil {
			t.Fatal("second acquisition must be refused while the first store holds the lock")
		}
		if !errors.Is(err, store.ErrInstanceLocked) {
			t.Fatalf("want ErrInstanceLocked, got: %v", err)
		}
	})
}

func TestRecordPolicyVersion(t *testing.T) {
	t.Run("Given a policy version hash, when recorded twice, assert that the policy table holds a single row for that version", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		hash := store.ContentHash([]byte("policy-content"))
		if err := s.RecordPolicyVersion(ctx, hash, []byte(`{"repositories":[]}`)); err != nil {
			t.Fatalf("first record failed: %v", err)
		}
		if err := s.RecordPolicyVersion(ctx, hash, []byte(`{"repositories":[]}`)); err != nil {
			t.Fatalf("second record failed: %v", err)
		}

		latest, err := s.LatestPolicyVersion(ctx)
		if err != nil {
			t.Fatalf("latest policy version failed: %v", err)
		}
		if latest.Version != hash {
			t.Fatalf("want version %s, got %s", hash, latest.Version)
		}
		if latest.AppliedAt.IsZero() {
			t.Fatal("applied timestamp must be set")
		}
	})

	t.Run("Given two distinct policy versions, when both are recorded, assert that latest returns the newer hash", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		old := store.ContentHash([]byte("policy-v1"))
		new := store.ContentHash([]byte("policy-v2"))
		if err := s.RecordPolicyVersion(ctx, old, []byte(`{}`)); err != nil {
			t.Fatalf("record old failed: %v", err)
		}
		if err := s.RecordPolicyVersion(ctx, new, []byte(`{}`)); err != nil {
			t.Fatalf("record new failed: %v", err)
		}

		latest, err := s.LatestPolicyVersion(ctx)
		if err != nil {
			t.Fatalf("latest policy version failed: %v", err)
		}
		if latest.Version != new {
			t.Fatalf("want latest version %s, got %s", new, latest.Version)
		}
	})

	t.Run("Given no policy recorded yet, when latest is queried, assert that it reports not-found instead of an error", func(t *testing.T) {
		s := openTestStore(t)
		ctx := context.Background()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate failed: %v", err)
		}

		_, err := s.LatestPolicyVersion(ctx)
		if !errors.Is(err, store.ErrPolicyNotFound) {
			t.Fatalf("want ErrPolicyNotFound, got: %v", err)
		}
	})
}
