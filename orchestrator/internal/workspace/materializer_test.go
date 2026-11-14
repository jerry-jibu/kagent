package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// seedTicket inserts one ticket row with the given status through a direct
// pool pinned to the store's schema (test fixture only; the Store keeps its
// pool private).
func seedTicket(t *testing.T, s *store.Store, id, status string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = s.Schema()
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open seed pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tickets (id, thread_source, thread_key, mandate, status)
		 VALUES ($1, 'github', $2, '{"policy_version":"test"}', $3)`,
		id, "key-"+id, status); err != nil {
		t.Fatalf("seed ticket %s: %v", id, err)
	}
}

// testDatabaseURL returns the DATABASE_URL value or skips the test.
// PG tests run against a real PostgreSQL; see CONTEXT.md for the setup.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL-backed test")
	}
	return url
}

// openFlowStore opens a store on a fresh schema for flow tests. The schema
// name must stay a short plain identifier: PostgreSQL identifiers truncate
// at 63 bytes and search_path elements beyond that do not resolve.
func openFlowStore(t *testing.T) *store.Store {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	schema := fmt.Sprintf("orch_ws_%08x_%d", h.Sum32(), time.Now().UnixNano()%1000000)
	ctx := context.Background()
	s, err := store.Open(ctx, store.Config{
		DatabaseURL: testDatabaseURL(t),
		Schema:      schema,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		s.DropSchema(context.Background())
		s.Close()
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func newFlowFixture(t *testing.T, ticketID, branch, token string) (*store.Store, *fakeAgentfs, *workspace.Materializer) {
	t.Helper()
	s := openFlowStore(t)
	fake := newFakeAgentfs()
	t.Cleanup(fake.close)
	m := workspace.NewMaterializer(workspace.MaterializerOptions{
		Cloner:  workspace.NewCloner(workspace.ClonerOptions{}),
		Backend: workspace.NewAgentfsBackend(fake.endpoint()),
		Store:   s,
	})
	seedTicket(t, s, ticketID, "accepted")
	return s, fake, m
}

func TestMaterializeTicketWorkspace(t *testing.T) {
	t.Run("Given an accepted ticket and a local repo with branch main, when the materializer runs, assert that the workspace receives the branch working copy byte-identical, the base commit is the branch HEAD and the run is audited", func(t *testing.T) {
		repo, files := newSourceRepo(t)
		head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "main"))
		s, fake, m := newFlowFixture(t, "ticket-ok", "main", "")

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := m.MaterializeTicketWorkspace(ctx, "ticket-ok", workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "main",
		})
		if err != nil {
			t.Fatalf("MaterializeTicketWorkspace failed: %v", err)
		}

		if res.Workspace != "ticket-ok" {
			t.Fatalf("result workspace = %q, want the ticket id", res.Workspace)
		}
		if res.BaseCommit != head {
			t.Fatalf("BaseCommit = %s, want %s", res.BaseCommit, head)
		}
		if res.Files != len(files) {
			t.Fatalf("result files = %d, want %d", res.Files, len(files))
		}

		// Read back through the same backend seam: byte-identical.
		backend := workspace.NewAgentfsBackend(fake.endpoint())
		for rel, want := range files {
			got, err := backend.ReadFile(ctx, "ticket-ok", rel)
			if err != nil {
				t.Fatalf("read-back of %s failed: %v", rel, err)
			}
			if !bytesEqual(got, want) {
				t.Fatalf("read-back of %s is not byte-identical", rel)
			}
		}

		// Audit + ticket state.
		events, err := s.AuditEvents(ctx, "ticket-ok")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 1 || events[0].Event != "workspace.materialized" {
			t.Fatalf("audit = %+v, want exactly one workspace.materialized record", events)
		}
		if events[0].Detail["base_commit"] != head {
			t.Fatalf("audit detail = %v, want base_commit %s", events[0].Detail, head)
		}
		status, err := s.TicketStatus(ctx, "ticket-ok")
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != "accepted" {
			t.Fatalf("ticket status = %q, want unchanged accepted", status)
		}
	})

	t.Run("Given a ticket and a repo whose branch does not exist, when the materializer runs, assert that it fails promptly, the ticket transitions to failed and the audit records the reason", func(t *testing.T) {
		repo, _ := newSourceRepo(t)
		s, _, m := newFlowFixture(t, "ticket-badbranch", "nope", "")

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		start := time.Now()
		_, err := m.MaterializeTicketWorkspace(ctx, "ticket-badbranch", workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "no-such-branch",
		})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("materialization of a missing branch succeeded, want failure")
		}
		if elapsed > 10*time.Second {
			t.Fatalf("flow took %s, want prompt failure (不挂起)", elapsed)
		}

		status, err := s.TicketStatus(ctx, "ticket-badbranch")
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != "failed" {
			t.Fatalf("ticket status = %q, want failed", status)
		}
		events, err := s.AuditEvents(ctx, "ticket-badbranch")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) != 1 || events[0].Event != "ticket.failed" {
			t.Fatalf("audit = %+v, want exactly one ticket.failed record", events)
		}
		reason, _ := events[0].Detail["reason"].(string)
		if !strings.Contains(reason, "no-such-branch") {
			t.Fatalf("failure reason = %q, want it to name the branch", reason)
		}
	})

	t.Run("Given a ticket id with no row, when the materializer hits a clone failure, assert that the returned error also surfaces the missing-ticket problem", func(t *testing.T) {
		repo, _ := newSourceRepo(t)
		_, _, m := newFlowFixture(t, "ticket-ghost-ok", "main", "")

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, err := m.MaterializeTicketWorkspace(ctx, "ticket-ghost-missing", workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "no-such-branch",
		})
		if err == nil {
			t.Fatal("flow with ghost ticket succeeded, want failure")
		}
		if !errors.Is(err, store.ErrTicketNotFound) {
			t.Fatalf("error = %v, want it to surface ErrTicketNotFound", err)
		}
	})
}
