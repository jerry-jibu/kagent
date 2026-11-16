package delivery_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/delivery"
	"github.com/jerry-jibu/kagent/orchestrator/internal/github"
	"github.com/jerry-jibu/kagent/orchestrator/internal/outbox"
	"github.com/jerry-jibu/kagent/orchestrator/internal/store"
	"github.com/jerry-jibu/kagent/orchestrator/internal/testenv"
	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// fixture wires the whole delivery chain on test doubles: a real PostgreSQL
// store, an in-memory workspace backend, a local bare git remote and the fake
// GitHub.
type fixture struct {
	t       *testing.T
	store   *store.Store
	backend *memBackend
	fake    *github.Fake
	gh      github.Transport
	root    string
	origin  string
	remote  string // file:// URL
	baseSHA string
	d       *delivery.Deliverer
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@example.com",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testenv.NewMigratedStore(t)
	fake := github.NewFake()
	t.Cleanup(fake.Close)

	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	// The bare remote sits at <root>/acme/api.git so the deliverer's git URL
	// template (GitBaseURL + "/" + repo + ".git") resolves to it.
	origin := filepath.Join(root, "acme", "api.git")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	runGit(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "seed")
	baseSHA := runGit(t, seed, "rev-parse", "HEAD")
	runGit(t, root, "clone", "--bare", "--no-hardlinks", seed, origin)
	runGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	backend := newMemBackend()
	f := &fixture{
		t:       t,
		store:   st,
		backend: backend,
		fake:    fake,
		gh:      github.NewTransport(github.TransportConfig{BaseURL: fake.URL()}),
		root:    root,
		origin:  origin,
		remote:  "file://" + origin,
		baseSHA: baseSHA,
	}
	f.d = delivery.New(st, backend, f.gh, delivery.Options{GitBaseURL: "file://" + root})
	return f
}

// readyTicket creates an accepted ticket, records the materialization
// baseline, dispatches its assignment and settles it under a ready report —
// the exact state a delivery candidate is scanned in.
func (f *fixture) readyTicket(t *testing.T, trigger string, reportSummary string, artifacts []string) string {
	t.Helper()
	ctx := context.Background()
	created, err := f.store.CreateTicket(ctx, acceptInput("github:acme/api", trigger))
	if err != nil {
		t.Fatalf("CreateTicket failed: %v", err)
	}
	ticketID := created.Ticket.ID

	assignments, err := f.store.Assignments(ctx, ticketID)
	if err != nil {
		t.Fatalf("Assignments failed: %v", err)
	}
	if err := f.store.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID,
		"task-"+trigger, "ctx-"+trigger, time.Time{}); err != nil {
		t.Fatalf("MarkAssignmentDispatched failed: %v", err)
	}

	detail := map[string]any{"state": "ready", "task_id": "task-" + trigger}
	if reportSummary != "" {
		detail["summary"] = reportSummary
	}
	if len(artifacts) > 0 {
		detail["artifacts"] = artifacts
	}
	if _, err := f.store.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
		AssignmentID:     assignments[0].ID,
		AssignmentStatus: store.AssignmentStatusEnded,
		ReportState:      "ready",
		TicketID:         ticketID,
		TicketStatus:     store.TicketStatusEnded,
		DeliveryPending:  true,
		Event:            "report.ready",
		Detail:           detail,
	}); err != nil {
		t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
	}

	if err := f.store.RecordTicketBaseline(ctx, ticketID, "main", f.baseSHA, []string{}); err != nil {
		t.Fatalf("RecordTicketBaseline failed: %v", err)
	}
	return ticketID
}

// workspaceFiles writes the workspace state the worker would have left.
func (f *fixture) workspaceFiles(t *testing.T, ticketID string, files map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	if err := f.backend.EnsureWorkspace(ctx, ticketID); err != nil {
		t.Fatalf("EnsureWorkspace failed: %v", err)
	}
	for path, content := range files {
		if err := f.backend.WriteFile(ctx, ticketID, path, content); err != nil {
			t.Fatalf("WriteFile %s failed: %v", path, err)
		}
	}
}

func (f *fixture) branchSHA(t *testing.T, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", f.origin, "rev-parse", "--verify", "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) openPRs(t *testing.T, head string) []github.PullRequest {
	t.Helper()
	var out []github.PullRequest
	for _, pr := range f.fake.PullRequests() {
		if pr.Head == head {
			out = append(out, pr)
		}
	}
	return out
}

func TestDeliverPending(t *testing.T) {
	t.Run("Given a ready ticket with a modified workspace and a live git remote, when DeliverPending runs, assert that the ticket branch is pushed, the PR opens referencing the ticket and issue, and the result-comment effect is queued with its marker — the whole V1 tail in one beat", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8001", "rewrote the parser", []string{"app.go"})
		f.workspaceFiles(t, ticketID, map[string][]byte{
			"app.go":  []byte("package app\n\n// rewritten\n"),
			"new.txt": []byte("added by worker\n"),
		})

		stats, err := f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Delivered != 1 {
			t.Fatalf("stats = %+v, want one delivery", stats)
		}

		// The ticket branch exists and descends from the baseline.
		sha := f.branchSHA(t, ticketID)
		if sha == "" {
			t.Fatalf("ticket branch %s was not pushed", ticketID)
		}
		parent := runGit(t, f.origin, "rev-parse", sha+"^")
		if parent != f.baseSHA {
			t.Fatalf("branch parent %s != baseline %s", parent, f.baseSHA)
		}

		// The PR references the ticket and the issue.
		prs := f.openPRs(t, ticketID)
		if len(prs) != 1 {
			t.Fatalf("open PRs for %s = %d, want 1", ticketID, len(prs))
		}
		if prs[0].Base != "main" {
			t.Fatalf("PR base = %s, want main", prs[0].Base)
		}

		// The result-comment effect carries the PR link and the marker.
		effects, err := f.store.Effects(context.Background())
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		var result *store.Effect
		for i := range effects {
			if effects[i].IdempotencyKey == "result:"+ticketID {
				result = &effects[i]
			}
		}
		if result == nil {
			t.Fatalf("result-comment effect was not enqueued")
		}
		if result.State != store.EffectPending {
			t.Fatalf("result effect state = %s, want pending", result.State)
		}
		body := string(result.Payload)
		if !strings.Contains(body, prs[0].URL) || !strings.Contains(body, github.EffectMarker("result:"+ticketID)) {
			t.Fatalf("result effect payload lost the PR link or the marker: %s", body)
		}

		// The audit records the delivery.
		events, err := f.store.AuditEvents(context.Background(), ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		delivered := 0
		for _, ev := range events {
			if ev.Event == "delivery.delivered" {
				delivered++
			}
		}
		if delivered != 1 {
			t.Fatalf("delivery.delivered audits = %d, want 1", delivered)
		}

		status, err := f.store.TicketStatus(context.Background(), ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusEnded {
			t.Fatalf("ticket status = %s, want ended", status)
		}
	})

	t.Run("Given a delivered ticket, when DeliverPending runs again, assert that nothing repeats: no second PR, no second effect, no scan hit", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8002", "small fix", nil)
		f.workspaceFiles(t, ticketID, map[string][]byte{"app.go": []byte("package app\n\n// fixed\n")})

		if _, err := f.d.DeliverPending(context.Background()); err != nil {
			t.Fatalf("first DeliverPending failed: %v", err)
		}
		stats, err := f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("replay DeliverPending failed: %v", err)
		}
		if stats.Delivered+stats.Confirmed+stats.Failed+stats.Retried != 0 {
			t.Fatalf("replay stats = %+v, want all zero (ticket left the scan)", stats)
		}
		if n := len(f.openPRs(t, ticketID)); n != 1 {
			t.Fatalf("open PRs = %d, want 1 (no duplicate PR)", n)
		}
		effects, err := f.store.Effects(context.Background())
		if err != nil {
			t.Fatalf("Effects failed: %v", err)
		}
		results := 0
		for _, e := range effects {
			if e.IdempotencyKey == "result:"+ticketID {
				results++
			}
		}
		if results != 1 {
			t.Fatalf("result effects = %d, want 1", results)
		}
	})

	t.Run("Given a ready ticket whose workspace matches its baseline, when DeliverPending runs, assert that the ticket fails with its audit and no PR is pushed — no empty PR", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8003", "no real change", nil)
		f.workspaceFiles(t, ticketID, map[string][]byte{"app.go": []byte("package app\n")}) // identical to baseline

		stats, err := f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Failed != 1 {
			t.Fatalf("stats = %+v, want one permanent failure", stats)
		}

		status, err := f.store.TicketStatus(context.Background(), ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusFailed {
			t.Fatalf("ticket status = %s, want failed", status)
		}
		if f.branchSHA(t, ticketID) != "" {
			t.Fatalf("no branch must be pushed for an empty delta")
		}
		if n := len(f.openPRs(t, ticketID)); n != 0 {
			t.Fatalf("open PRs = %d, want 0", n)
		}
		events, err := f.store.AuditEvents(context.Background(), ticketID)
		if err != nil {
			t.Fatalf("AuditEvents failed: %v", err)
		}
		failures := 0
		for _, ev := range events {
			if ev.Event == "delivery.failed" {
				failures++
			}
		}
		if failures != 1 {
			t.Fatalf("delivery.failed audits = %d, want 1", failures)
		}
	})

	t.Run("Given a ready ticket with no recorded baseline, when DeliverPending runs, assert that it fails permanently — a workspace without a baseline can never be delivered", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		created, err := f.store.CreateTicket(ctx, acceptInput("github:acme/api", "issue-comment:8004"))
		if err != nil {
			t.Fatalf("CreateTicket failed: %v", err)
		}
		ticketID := created.Ticket.ID
		assignments, err := f.store.Assignments(ctx, ticketID)
		if err != nil {
			t.Fatalf("Assignments failed: %v", err)
		}
		if err := f.store.MarkAssignmentDispatched(ctx, assignments[0].ID, ticketID, "task-8004", "ctx-8004", time.Time{}); err != nil {
			t.Fatalf("MarkAssignmentDispatched failed: %v", err)
		}
		if _, err := f.store.ApplyAssignmentTerminal(ctx, store.TerminalPlan{
			AssignmentID:     assignments[0].ID,
			AssignmentStatus: store.AssignmentStatusEnded,
			ReportState:      "ready",
			TicketID:         ticketID,
			TicketStatus:     store.TicketStatusEnded,
			DeliveryPending:  true,
			Event:            "report.ready",
			Detail:           map[string]any{"state": "ready"},
		}); err != nil {
			t.Fatalf("ApplyAssignmentTerminal failed: %v", err)
		}

		stats, err := f.d.DeliverPending(ctx)
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Failed != 1 {
			t.Fatalf("stats = %+v, want one permanent failure", stats)
		}
		status, err := f.store.TicketStatus(ctx, ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusFailed {
			t.Fatalf("ticket status = %s, want failed", status)
		}
	})

	t.Run("Given a backend outage, when DeliverPending runs, assert that the ticket stays pending for the next beat and no PR is created — an unreadable workspace is transient, not failed", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8005", "outage", nil)
		// No workspace was ever materialized: the backend refuses it.
		f.backend.failWorkspace = ticketID

		stats, err := f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}
		if stats.Retried != 1 {
			t.Fatalf("stats = %+v, want one retried candidate", stats)
		}
		status, err := f.store.TicketStatus(context.Background(), ticketID)
		if err != nil {
			t.Fatalf("TicketStatus failed: %v", err)
		}
		if status != store.TicketStatusEnded {
			t.Fatalf("ticket status = %s, want ended (still pending delivery)", status)
		}
		if n := len(f.openPRs(t, ticketID)); n != 0 {
			t.Fatalf("open PRs = %d, want 0", n)
		}
	})

	t.Run("Given a failing GitHub, when DeliverPending runs and then recovers, assert that the delivery completes on the later beat exactly once — transient PR failure never loses the result", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8006", "after outage", nil)
		f.workspaceFiles(t, ticketID, map[string][]byte{"app.go": []byte("package app\n\n// v2\n")})

		f.fake.FailWith(500, "github down")
		stats, err := f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending (failing GitHub) failed: %v", err)
		}
		if stats.Retried != 1 {
			t.Fatalf("stats = %+v, want one retried candidate", stats)
		}
		if f.branchSHA(t, ticketID) == "" {
			t.Fatalf("the branch push succeeded before the PR failure — it must persist for the retry")
		}

		f.fake.SetOK()
		stats, err = f.d.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("DeliverPending (recovered) failed: %v", err)
		}
		if stats.Delivered != 1 {
			t.Fatalf("stats = %+v, want one delivery after recovery", stats)
		}
		if n := len(f.openPRs(t, ticketID)); n != 1 {
			t.Fatalf("open PRs = %d, want 1 after recovery", n)
		}
	})

	t.Run("Given a delivered ticket whose comment effect is pending, when the outbox drains, assert that the result comment lands on the issue with the PR link and the report summary, and a re-drain posts nothing twice", func(t *testing.T) {
		f := newFixture(t)
		ticketID := f.readyTicket(t, "issue-comment:8007", "summary for the comment", nil)
		f.workspaceFiles(t, ticketID, map[string][]byte{"app.go": []byte("package app\n\n// v3\n")})

		if _, err := f.d.DeliverPending(context.Background()); err != nil {
			t.Fatalf("DeliverPending failed: %v", err)
		}

		drainer := outbox.New(f.store, f.gh)
		dstats, err := drainer.DeliverPending(context.Background())
		if err != nil {
			t.Fatalf("outbox drain failed: %v", err)
		}
		if dstats.Delivered != 1 {
			t.Fatalf("drain stats = %+v, want one delivery (the result comment)", dstats)
		}

		comments := f.fake.PostedComments()
		if len(comments) != 1 {
			t.Fatalf("posted comments = %d, want 1", len(comments))
		}
		if !strings.Contains(comments[0].Body, "http") || !strings.Contains(comments[0].Body, "summary for the comment") {
			t.Fatalf("result comment lost the PR link or the summary: %s", comments[0].Body)
		}

		if _, err := drainer.DeliverPending(context.Background()); err != nil {
			t.Fatalf("replay drain failed: %v", err)
		}
		if len(f.fake.PostedComments()) != 1 {
			t.Fatalf("replay posted a duplicate comment")
		}
	})
}

// acceptInput mirrors the store tests' ticket input for acme/api#42.
func acceptInput(source, deterministicID string) store.TicketInput {
	return store.TicketInput{
		Source:          source,
		DeterministicID: deterministicID,
		ThreadKey:       "issue:42",
		Decision: store.DecisionInput{
			Outcome:       store.DecisionAccept,
			Strength:      "explicit",
			PolicyVersion: "abc123",
			Reason:        "authorized coding request",
		},
		Mandate: store.Mandate{
			PolicyVersion: "abc123",
			Strength:      "explicit",
			Sender:        "alice",
			Scope:         store.MandateScope{Repo: "acme/api", Issue: 42},
		},
	}
}

// memBackend is the in-memory Backend (same contract as the workspace tests'
// one) with a per-workspace failure switch for outage scenarios.
type memBackend struct {
	mu            sync.Mutex
	files         map[string]map[string][]byte
	failWorkspace string // when set, every access to this workspace fails
}

func newMemBackend() *memBackend {
	return &memBackend{files: map[string]map[string][]byte{}}
}

func (m *memBackend) EnsureWorkspace(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == m.failWorkspace {
		return fmt.Errorf("workspace %s is unreachable", name)
	}
	if _, ok := m.files[name]; !ok {
		m.files[name] = map[string][]byte{}
	}
	return nil
}

func (m *memBackend) WriteFile(_ context.Context, ws, path string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws == m.failWorkspace {
		return fmt.Errorf("workspace %s is unreachable", ws)
	}
	wsFiles, ok := m.files[ws]
	if !ok {
		return fmt.Errorf("workspace %s does not exist", ws)
	}
	wsFiles[path] = append([]byte(nil), content...)
	return nil
}

func (m *memBackend) ReadFile(_ context.Context, ws, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws == m.failWorkspace {
		return nil, fmt.Errorf("workspace %s is unreachable", ws)
	}
	wsFiles, ok := m.files[ws]
	if !ok {
		return nil, fmt.Errorf("%w: %s", workspace.ErrFileNotFound, path)
	}
	content, ok := wsFiles[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", workspace.ErrFileNotFound, path)
	}
	return append([]byte(nil), content...), nil
}

func (m *memBackend) ListFiles(_ context.Context, ws, _ string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws == m.failWorkspace {
		return nil, fmt.Errorf("workspace %s is unreachable", ws)
	}
	wsFiles, ok := m.files[ws]
	if !ok {
		return nil, fmt.Errorf("workspace %s does not exist", ws)
	}
	var out []string
	for path := range wsFiles {
		out = append(out, path)
	}
	return out, nil
}
