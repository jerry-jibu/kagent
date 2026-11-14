package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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
			"outbox",
			"policy",
			"schema_migrations",
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
		if len(versions) != 2 || versions[0] != "0001_init" || versions[1] != "0002_audit" {
			t.Fatalf("expected [0001_init 0002_audit] recorded, got %v", versions)
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
		if len(versions) != 2 {
			t.Fatalf("expected each migration recorded exactly once, got %v", versions)
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
