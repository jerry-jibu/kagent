package store

// Migration 0010's schema contract, inspected through the raw catalogs: the
// hot-path indexes exist after the migration and the rate-limit deferral
// column is present (PR #11 review, Required #3). The behavior the column
// enables — PendingDeliveries filtering — is covered in delivery_test.go.

import (
	"context"
	"testing"
)

func TestHotPathSchema(t *testing.T) {
	t.Run("Given a fully migrated schema, when the 0010 indexes and column are inspected, assert that the hot-path indexes and rate_limited_until exist", func(t *testing.T) {
		s := openInternalTestStore(t)
		mustMigrateInternal(t, s)
		ctx := context.Background()

		rows, err := s.pool.Query(ctx, `
			SELECT indexname FROM pg_indexes WHERE schemaname = $1`, s.schema)
		if err != nil {
			t.Fatalf("failed to list indexes: %v", err)
		}
		defer rows.Close()
		indexes := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("failed to scan index name: %v", err)
			}
			indexes[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("failed to iterate indexes: %v", err)
		}

		for _, want := range []string{
			"assignments_ticket_created_idx", // latest-assignment CTE + Assignments(ticket)
			"triggers_thread_idx",            // waitsForPerson "nobody spoke since" probe
			"tickets_thread_idx",             // FollowupOwnerOfThread + ingest sightline probe
		} {
			if !indexes[want] {
				t.Fatalf("hot-path index %s missing after migration; have %v", want, indexes)
			}
		}

		var exists bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'tickets'
				  AND column_name = 'rate_limited_until')`, s.schema).Scan(&exists); err != nil {
			t.Fatalf("failed to inspect tickets columns: %v", err)
		}
		if !exists {
			t.Fatal("tickets.rate_limited_until missing after migration")
		}
	})
}
