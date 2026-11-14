package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// seedScopedTicket inserts one ticket row whose mandate names a scope repo,
// the shape the materialize scan composes the clone URL from (test fixture
// only; CreateTicket validation normally guarantees this shape).
func seedScopedTicket(t *testing.T, s *store.Store, id, status, repo string) {
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
	mandate := `{"policy_version":"test","strength":"explicit","sender":"maintainer","scope":{"repo":"` + repo + `","issue":7}}`
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO tickets (id, thread_source, thread_key, mandate, status)
		 VALUES ($1, 'github', $2, $3, $4)`,
		id, "key-"+id, mandate, status); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("seed scoped ticket %s: %v", id, err)
	}
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO assignments (id, ticket_id, status) VALUES ($1, $2, $3)`,
		"assign-"+id, id, store.AssignmentStatusPending); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("seed pending assignment for %s: %v", id, err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// newGitBaseRepo creates a local repository at <dir>/<repo>.git so the scan's
// URL composition (git base + "/" + repo + ".git") resolves against it, and
// returns the git base, the branch HEAD and the branch files.
func newGitBaseRepo(t *testing.T, repo string) (string, string, map[string][]byte) {
	t.Helper()
	base := t.TempDir()
	repoDir := filepath.Join(base, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}
	gitRun(t, repoDir, "init", "-q", "-b", "main")
	files := map[string][]byte{
		"README.md":   []byte("# demo repo\n"),
		"src/main.go": []byte("package main\n\nfunc main() {}\n"),
	}
	for rel, content := range files {
		p := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repoDir, "add", ".")
	gitRun(t, repoDir, "-c", "user.email=orc@test", "-c", "user.name=orc", "commit", "-m", "initial", "-q")
	head := strings.TrimSpace(gitRun(t, repoDir, "rev-parse", "main"))
	return "file://" + base, head, files
}

func TestMaterializePending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	newScanFixture := func(t *testing.T, gitBase string) (*store.Store, *fakeAgentfs, *workspace.Materializer) {
		t.Helper()
		s := openFlowStore(t)
		fake := newFakeAgentfs()
		t.Cleanup(fake.close)
		m := workspace.NewMaterializer(workspace.MaterializerOptions{
			Cloner:     workspace.NewCloner(workspace.ClonerOptions{}),
			Backend:    workspace.NewAgentfsBackend(fake.endpoint()),
			Store:      s,
			GitBaseURL: gitBase,
			BaseBranch: "main",
		})
		return s, fake, m
	}

	t.Run("Given an accepted ticket whose mandate names a scope repo, when the materialize scan runs against a local git base, assert that the workspace is materialized, the dispatch-gated scan sees the ticket and the run is audited (accept→物化→派工 链闭合)", func(t *testing.T) {
		gitBase, _, files := newGitBaseRepo(t, "acme/api")
		s, fake, m := newScanFixture(t, gitBase)
		seedScopedTicket(t, s, "ticket-scan-ok", store.TicketStatusPending, "acme/api")
		materializer := m

		stats, err := materializer.MaterializePending(ctx)
		if err != nil {
			t.Fatalf("MaterializePending failed: %v", err)
		}
		if stats.Materialized != 1 || stats.Failed != 0 {
			t.Fatalf("stats = %+v, want 1 materialized / 0 failed", stats)
		}

		// The workspace carries the branch working copy.
		backend := workspace.NewAgentfsBackend(fake.endpoint())
		for rel, want := range files {
			got, err := backend.ReadFile(ctx, "ticket-scan-ok", rel)
			if err != nil {
				t.Fatalf("read-back of %s failed: %v", rel, err)
			}
			if !bytesEqual(got, want) {
				t.Fatalf("read-back of %s is not byte-identical", rel)
			}
		}

		// The chain closes: with the workspace gate on, the pending
		// assignment is now dispatchable.
		pending, err := s.PendingDispatches(ctx, true)
		if err != nil {
			t.Fatalf("PendingDispatches(true) failed: %v", err)
		}
		if len(pending) != 1 || pending[0].Ticket.ID != "ticket-scan-ok" {
			t.Fatalf("gated dispatch scan = %+v, want ticket-scan-ok dispatchable after materialization", pending)
		}

		events, err := s.AuditEvents(ctx, "ticket-scan-ok")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(events) == 0 || events[len(events)-1].Event != "workspace.materialized" {
			t.Fatalf("audit trail = %+v, want a workspace.materialized row", events)
		}
	})

	t.Run("Given a healthy ticket and one whose scope repo does not exist, when the scan runs, assert that the failure transitions only its own ticket and the healthy ticket still materializes", func(t *testing.T) {
		gitBase, _, _ := newGitBaseRepo(t, "acme/api")
		s, _, m := newScanFixture(t, gitBase)
		seedScopedTicket(t, s, "ticket-scan-good", store.TicketStatusPending, "acme/api")
		seedScopedTicket(t, s, "ticket-scan-broken", store.TicketStatusPending, "acme/missing")
		materializer := m

		stats, err := materializer.MaterializePending(ctx)
		if err != nil {
			t.Fatalf("MaterializePending failed: %v", err)
		}
		if stats.Materialized != 1 || stats.Failed != 1 {
			t.Fatalf("stats = %+v, want 1 materialized / 1 failed", stats)
		}
		goodStatus, err := s.TicketStatus(ctx, "ticket-scan-good")
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if goodStatus != store.TicketStatusPending {
			t.Fatalf("healthy ticket status = %q, want still pending", goodStatus)
		}
		badStatus, err := s.TicketStatus(ctx, "ticket-scan-broken")
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if badStatus != store.TicketStatusFailed {
			t.Fatalf("broken ticket status = %q, want failed", badStatus)
		}
	})

	t.Run("Given a ticket whose baseline already landed, when the scan runs again, assert that the workspace is not re-materialized and no duplicate audit row appears (restart-rescan idempotence)", func(t *testing.T) {
		gitBase, _, _ := newGitBaseRepo(t, "acme/api")
		s, fake, m := newScanFixture(t, gitBase)
		seedScopedTicket(t, s, "ticket-scan-done", store.TicketStatusPending, "acme/api")
		materializer := m

		if _, err := materializer.MaterializePending(ctx); err != nil {
			t.Fatalf("first scan failed: %v", err)
		}
		callsAfterFirst := len(fake.callNames())
		events, err := s.AuditEvents(ctx, "ticket-scan-done")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}

		stats, err := materializer.MaterializePending(ctx)
		if err != nil {
			t.Fatalf("second scan failed: %v", err)
		}
		if stats.Materialized != 0 || stats.Failed != 0 {
			t.Fatalf("second scan stats = %+v, want nothing to do", stats)
		}
		if got := len(fake.callNames()); got != callsAfterFirst {
			t.Fatalf("bridge calls went %d → %d, want no re-materialization", callsAfterFirst, got)
		}
		after, err := s.AuditEvents(ctx, "ticket-scan-done")
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		if len(after) != len(events) {
			t.Fatalf("audit rows went %d → %d, want no duplicate", len(events), len(after))
		}
	})
}
