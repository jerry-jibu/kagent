// Package testenv provides PostgreSQL-backed test fixtures shared by
// orchestrator packages: a DATABASE_URL skip guard and a Store on a fresh,
// isolated per-test schema.
package testenv

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
)

// DatabaseURL returns the DATABASE_URL value or skips the test. PG tests run
// against a real PostgreSQL; see the agent context notes for the local setup.
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL test")
	}
	return url
}

// NewMigratedStore opens a Store on a fresh, uniquely named schema and
// applies all migrations. The schema is dropped on test cleanup, so tests
// never observe each other's state.
func NewMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, store.Config{
		DatabaseURL: DatabaseURL(t),
		Schema:      fmt.Sprintf("orch_test_%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() {
		s.DropSchema(context.Background())
		s.Close()
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}
	return s
}
