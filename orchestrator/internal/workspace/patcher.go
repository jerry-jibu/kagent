package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrEmptyChangeSet reports that the workspace state matches its baseline:
// there is nothing to deliver, and pushing a PR from it would be an empty PR.
// The deliverer fails the ticket permanently instead.
var ErrEmptyChangeSet = errors.New("workspace state matches the baseline; nothing to deliver")

// Commit identity for the delivered patch. The orchestrator authors the
// commit (Alternative E: the worker never touches git), so the identity names
// it. Overridable per request for deployments that want a project-specific
// author.
const (
	DefaultCommitterName  = "kagent-orchestrator"
	DefaultCommitterEmail = "orchestrator@kagent-dev.local"
)

// PatchRequest is one delivery patch: the workspace read-back state applied
// onto the exact materialization baseline, committed to the ticket branch and
// pushed (0003 Alternative E: the orchestrator computes and ships the patch;
// the worker never produces one).
type PatchRequest struct {
	// RepoURL is the clone/push URL. It must be credential-free
	// (ValidateRepoURL enforces); the token rides the credential helper.
	RepoURL string
	// Token is the orchestrator-held git credential. Optional for local and
	// public remotes.
	Token string
	// BaseCommit is the full SHA the workspace was materialized from. The
	// ticket branch descends from it — never from the (possibly moved)
	// upstream tip.
	BaseCommit string
	// Branch is the ticket branch name (refs/heads/<Branch>); the P0
	// convention is branch name = ticket ID. Validated as a safe ref.
	Branch string
	// Message is the commit message; it must reference the ticket (and the
	// originating issue) so the PR is traceable.
	Message string
	// State is the workspace read-back snapshot (ReadWorkspaceState): every
	// path currently in the workspace with its byte-identical content.
	State map[string][]byte
	// SkipPaths are baseline paths materialization could not transport
	// (binary, oversized, symlink). Their absence from State means "never
	// materialized", never "worker deleted" — they are never removed from
	// the branch tree.
	SkipPaths []string
	// AuthorName/AuthorEmail override the commit identity; empty uses the
	// defaults.
	AuthorName  string
	AuthorEmail string
}

// PatchOutcome reports one shipped patch.
type PatchOutcome struct {
	// Commit is the SHA of the commit pushed to refs/heads/<Branch>.
	Commit string
}

// PatcherOptions tune the patcher.
type PatcherOptions struct {
	// Timeout bounds one whole patch run (clone + apply + commit + push);
	// zero uses the default.
	Timeout time.Duration
}

// Patcher ships one ticket's workspace delta to the remote as a ticket branch
// (docs/designs/0003 Alternative E). Git mechanics: clone the exact baseline
// commit, write the workspace state over it, delete baseline files the
// workspace no longer holds (minus the untransportable ones), commit and push
// the branch. The PR itself is the caller's (GitHubTransport extension).
type Patcher struct {
	cloner  *Cloner
	timeout time.Duration
}

// NewPatcher builds a patcher with its own baseline cloner (the credential
// and timeout discipline stay inside the workspace package).
func NewPatcher(opts PatcherOptions) *Patcher {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute // a shallow clone plus a tree write plus a push
	}
	return &Patcher{
		cloner:  NewCloner(ClonerOptions{}),
		timeout: timeout,
	}
}

// Patch applies req.State onto the baseline commit and pushes the ticket
// branch. It is idempotent for replays: the commit's author/committer dates
// are pinned to the baseline commit's committer date, so re-running Patch
// with the same state produces the SAME commit SHA and the push is a remote
// no-op ("Everything up-to-date") — a delivery retried after a lost receipt
// never diverges the branch.
func (p *Patcher) Patch(ctx context.Context, req PatchRequest) (PatchOutcome, error) {
	if err := ValidateRepoURL(req.RepoURL); err != nil {
		return PatchOutcome{}, err
	}
	if err := validateBranchName(req.Branch); err != nil {
		return PatchOutcome{}, err
	}
	if !isCommitSHA(req.BaseCommit) {
		return PatchOutcome{}, fmt.Errorf("base commit %q is not a full hex SHA", req.BaseCommit)
	}
	if req.State == nil {
		return PatchOutcome{}, fmt.Errorf("patch state is nil; read the workspace back first")
	}
	if strings.TrimSpace(req.Message) == "" {
		return PatchOutcome{}, fmt.Errorf("patch message is empty")
	}
	for path := range req.State {
		if err := validateWorkspacePath(path); err != nil {
			return PatchOutcome{}, fmt.Errorf("patch state carries an invalid path: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	baseline, err := p.cloner.CloneCommit(ctx, CloneCommitRequest{
		RepoURL: req.RepoURL,
		Commit:  req.BaseCommit,
		Token:   req.Token,
	})
	if err != nil {
		return PatchOutcome{}, err
	}
	defer os.RemoveAll(baseline.Dir)

	if err := applyState(baseline.Dir, req.State); err != nil {
		return PatchOutcome{}, err
	}
	if err := applyDeletions(baseline.Dir, req.State, skipSet(req.SkipPaths)); err != nil {
		return PatchOutcome{}, err
	}

	if out, err := gitRun(ctx, baseline.Dir, "", "add", "-A"); err != nil {
		return PatchOutcome{}, fmt.Errorf("failed to stage the workspace delta: %w: %s", err, tailOutput(out))
	}
	if out, err := gitRun(ctx, baseline.Dir, "", "status", "--porcelain"); err != nil {
		return PatchOutcome{}, fmt.Errorf("failed to inspect the staged delta: %w: %s", err, tailOutput(out))
	} else if strings.TrimSpace(string(out)) == "" {
		return PatchOutcome{}, ErrEmptyChangeSet
	}

	name := req.AuthorName
	if name == "" {
		name = DefaultCommitterName
	}
	email := req.AuthorEmail
	if email == "" {
		email = DefaultCommitterEmail
	}
	// Deterministic commit dates: derived from the baseline commit so a
	// replayed Patch with the same state yields the same SHA (see the doc
	// comment). Without this, each retry would mint a fresh timestamp and a
	// diverged branch.
	dateOut, err := gitRun(ctx, baseline.Dir, "", "log", "-1", "--format=%cI")
	if err != nil {
		return PatchOutcome{}, fmt.Errorf("failed to read the baseline commit date: %w: %s", err, tailOutput(dateOut))
	}
	baseDate := strings.TrimSpace(string(dateOut))
	commitEnv := []string{
		"GIT_AUTHOR_DATE=" + baseDate,
		"GIT_COMMITTER_DATE=" + baseDate,
	}
	commitArgs := []string{
		"-c", "user.name=" + name,
		"-c", "user.email=" + email,
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", req.Message,
	}
	if out, err := gitRunEnv(ctx, baseline.Dir, "", commitEnv, commitArgs...); err != nil {
		return PatchOutcome{}, fmt.Errorf("failed to commit the workspace delta: %w: %s", err, tailOutput(out))
	}

	head, err := resolveHead(ctx, baseline.Dir)
	if err != nil {
		return PatchOutcome{}, err
	}

	pushArgs := append(authArgs(), "push", "--quiet", req.RepoURL, "HEAD:refs/heads/"+req.Branch)
	if out, err := gitRun(ctx, baseline.Dir, req.Token, pushArgs...); err != nil {
		return PatchOutcome{}, fmt.Errorf("failed to push the ticket branch %s: %w: %s",
			req.Branch, err, tailOutput(out))
	}

	return PatchOutcome{Commit: head}, nil
}

// applyState writes every state file over the baseline tree, creating parent
// directories. Writing an unchanged file with identical content is a no-op
// for git — the commit captures only real deltas.
func applyState(dir string, state map[string][]byte) error {
	for rel, content := range state {
		if content == nil {
			return fmt.Errorf("patch state for %s is nil; the read-back snapshot never yields nil content", rel)
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("failed to create directories for %s: %w", rel, err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return fmt.Errorf("failed to write %s onto the baseline: %w", rel, err)
		}
	}
	return nil
}

// applyDeletions removes baseline files the workspace no longer holds. The
// walk skips .git (never a workspace concern) and every skip path
// (untransportable at materialization — absence is not deletion).
func applyDeletions(dir string, state map[string][]byte, skip map[string]bool) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if _, held := state[relSlash]; held {
			return nil
		}
		if skip[relSlash] {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to remove %s from the baseline tree: %w", relSlash, err)
		}
		return nil
	})
}

func skipSet(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		out[p] = true
	}
	return out
}

// validateBranchName keeps the caller-supplied branch name a safe ref: no
// traversal, no option injection, no ref-hostile characters.
func validateBranchName(branch string) error {
	switch {
	case branch == "":
		return fmt.Errorf("ticket branch name is empty")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("ticket branch name %q starts with '-': not a safe ref", branch)
	case strings.Contains(branch, ".."), strings.HasPrefix(branch, "/"), strings.HasSuffix(branch, "/"):
		return fmt.Errorf("ticket branch name %q is not a safe ref", branch)
	case strings.HasSuffix(branch, ".lock"), strings.Contains(branch, "//"):
		return fmt.Errorf("ticket branch name %q is not a safe ref", branch)
	}
	for _, r := range branch {
		if r <= ' ' || r > '~' || strings.ContainsRune("~^:?*[\\", r) {
			return fmt.Errorf("ticket branch name %q carries an unusable character %q", branch, r)
		}
	}
	return nil
}
