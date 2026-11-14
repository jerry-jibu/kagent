package workspace

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tokenEnvVar carries the git credential to the clone subprocess. The token
// travels ONLY through this environment variable — it never appears in the
// git argv, on disk, or inside the cloned repository (凭证单调, docs/designs/
// 0003: credentials stay in the orchestrator).
const tokenEnvVar = "ORCHESTRATOR_GIT_TOKEN"

// defaultCloneTimeout bounds a shallow clone; a clone that outlives it fails
// the materialization instead of hanging the ticket (0003 V6 cadence).
const defaultCloneTimeout = 5 * time.Minute

// inlineCredentialHelper is the transient git credential helper. Git
// evaluates it via sh with the clone subprocess env, so the token is read
// from tokenEnvVar at auth time and never persisted. The empty helper
// value before it resets any ambient credential helpers (e.g. osxkeychain)
// so nothing else can capture the credential.
const inlineCredentialHelper = `!f(){ echo "username=x-access-token"; echo "password=$` + tokenEnvVar + `"; }; f`

// CloneRequest identifies the shallow-clone source for one ticket.
type CloneRequest struct {
	// RepoURL is the clone URL. It must be clean: no credentials embedded
	// (ValidateRepoURL enforces this before any git invocation).
	RepoURL string
	// Branch is the issue-pointed base branch; the clone is pinned to its
	// HEAD (0003 路线 A: 浅克隆基线 = issue 指向分支 HEAD).
	Branch string
	// Token is the orchestrator-held git credential. Optional: local and
	// public repos clone without it.
	Token string
}

// CloneOutcome reports one finished shallow clone.
type CloneOutcome struct {
	// Dir is the clone root (a fresh temp directory). The caller owns its
	// cleanup; MaterializeTicketWorkspace removes it when done.
	Dir string
	// BaseCommit is the resolved HEAD SHA of the cloned branch.
	BaseCommit string
}

// ClonerOptions tune the cloner.
type ClonerOptions struct {
	// Timeout bounds a single clone; zero uses defaultCloneTimeout.
	Timeout time.Duration
}

// Cloner performs depth-1 single-branch shallow clones with the system git
// binary (allowed by the T5 contract; no extra Go dependency). It is the
// only place git credentials are touched: they ride the subprocess
// environment and a transient credential helper, never argv, never disk.
type Cloner struct {
	timeout time.Duration
}

// NewCloner builds a cloner.
func NewCloner(opts ClonerOptions) *Cloner {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultCloneTimeout
	}
	return &Cloner{timeout: timeout}
}

// Clone shallow-clones req.Branch at req.RepoURL into a fresh temp dir and
// returns the outcome. Errors are prompt (no retries, no prompts): a failed
// clone must fail the ticket, not hang it.
func (c *Cloner) Clone(ctx context.Context, req CloneRequest) (CloneOutcome, error) {
	if err := ValidateRepoURL(req.RepoURL); err != nil {
		return CloneOutcome{}, err
	}
	if strings.TrimSpace(req.Branch) == "" {
		return CloneOutcome{}, fmt.Errorf("clone branch is empty")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return CloneOutcome{}, fmt.Errorf("git binary not found on PATH: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dest, err := os.MkdirTemp("", "kagent-clone-")
	if err != nil {
		return CloneOutcome{}, fmt.Errorf("failed to create clone directory: %w", err)
	}

	if out, err := gitRun(ctx, dest, req.Token, BuildCloneArgs(req.RepoURL, req.Branch, dest)...); err != nil {
		_ = os.RemoveAll(dest)
		return CloneOutcome{}, fmt.Errorf("shallow clone of %s branch %s failed: %w: %s",
			req.RepoURL, req.Branch, err, tailOutput(out))
	}

	head, err := resolveHead(ctx, dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return CloneOutcome{}, err
	}
	return CloneOutcome{Dir: dest, BaseCommit: head}, nil
}

// CloneCommitRequest pins the fetch to one exact commit instead of a branch
// tip. The delivery diff (T7, 0003 Alternative E) is computed against the
// commit the workspace was MATERIALIZED from — if the upstream branch moved
// since, re-cloning the tip would fold other people's changes into the PR.
type CloneCommitRequest struct {
	// RepoURL is the clone URL (credential-free; ValidateRepoURL enforces).
	RepoURL string
	// Commit is the full 40-hex SHA to fetch.
	Commit string
	// Token is the orchestrator-held credential; optional.
	Token string
}

// CloneCommit fetches exactly req.Commit (depth 1, credential-free URL, token
// via the transient helper) into a fresh temp dir checked out at that commit
// (detached HEAD). GitHub accepts fetch-by-SHA server-side
// (uploadpack.allowAnySHA1InWant); local test remotes enable it in their
// config. Errors are prompt, like Clone.
func (c *Cloner) CloneCommit(ctx context.Context, req CloneCommitRequest) (CloneOutcome, error) {
	if err := ValidateRepoURL(req.RepoURL); err != nil {
		return CloneOutcome{}, err
	}
	if !isCommitSHA(req.Commit) {
		return CloneOutcome{}, fmt.Errorf("clone commit %q is not a full hex SHA", req.Commit)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return CloneOutcome{}, fmt.Errorf("git binary not found on PATH: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dest, err := os.MkdirTemp("", "kagent-clone-commit-")
	if err != nil {
		return CloneOutcome{}, fmt.Errorf("failed to create clone directory: %w", err)
	}

	cleanup := func(err error) (CloneOutcome, error) {
		_ = os.RemoveAll(dest)
		return CloneOutcome{}, err
	}

	if out, err := gitRun(ctx, dest, req.Token, "init", "-q", "."); err != nil {
		return cleanup(fmt.Errorf("failed to init the baseline clone: %w: %s", err, tailOutput(out)))
	}
	// -c options are git GLOBAL options: they must precede the subcommand.
	if out, err := gitRun(ctx, dest, req.Token,
		append(authArgs(), "fetch", "-q", "--depth", "1", "--no-tags", req.RepoURL, req.Commit)...); err != nil {
		return cleanup(fmt.Errorf("failed to fetch baseline commit %s of %s: %w: %s",
			req.Commit, req.RepoURL, err, tailOutput(out)))
	}
	if out, err := gitRun(ctx, dest, "", "checkout", "--detach", "--quiet", "FETCH_HEAD"); err != nil {
		return cleanup(fmt.Errorf("failed to check out the baseline commit: %w: %s", err, tailOutput(out)))
	}

	return CloneOutcome{Dir: dest, BaseCommit: req.Commit}, nil
}

// BuildCloneArgs returns the git argv for the shallow clone: depth 1,
// single branch, no tags, transient credential helper. The token is never
// part of argv — the helper reads it from the subprocess environment.
func BuildCloneArgs(repoURL, branch, dest string) []string {
	return append(authArgs(),
		"clone",
		"--depth", "1",
		"--single-branch",
		"--no-tags",
		"--branch", branch,
		repoURL,
		dest,
	)
}

// authArgs resets ambient credential helpers and installs the transient
// inline helper that reads the token from tokenEnvVar at auth time. Only
// meaningful for network transports; harmless for file://.
func authArgs() []string {
	return []string{
		"-c", "credential.helper=", // reset ambient helpers
		"-c", "credential.helper=" + inlineCredentialHelper,
	}
}

// gitRun runs one git command in dir with the orchestrator's hardened
// environment: no terminal prompts, and the token (when set) riding the
// credential-helper environment variable — never argv, never disk.
func gitRun(ctx context.Context, dir, token string, args ...string) ([]byte, error) {
	return gitRunEnv(ctx, dir, token, nil, args...)
}

// gitRunEnv is gitRun with extra environment for the subprocess (e.g. the
// deterministic commit dates the patcher pins).
func gitRunEnv(ctx context.Context, dir, token string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never hang on an interactive prompt
	)
	if token != "" {
		cmd.Env = append(cmd.Env, tokenEnvVar+"="+token)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

// isCommitSHA reports whether s is a full 40-hex commit SHA.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// ValidateRepoURL refuses clone URLs that embed credentials: a token inside
// a URL would be persisted into the cloned repository's .git/config by git,
// violating the worker-has-no-credentials contract (0002 A5).
func ValidateRepoURL(repoURL string) error {
	u, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("repo URL %q is not parseable: %w", repoURL, err)
	}
	if u.User != nil {
		return fmt.Errorf("repo URL %q embeds credentials; pass the token through CloneRequest.Token instead", repoURL)
	}
	switch u.Scheme {
	case "https", "file":
		return nil
	default:
		return fmt.Errorf("repo URL scheme %q is not supported; use https", u.Scheme)
	}
}

// resolveHead reads the cloned branch's HEAD SHA.
func resolveHead(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve cloned HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// tailOutput keeps error text bounded.
func tailOutput(out []byte) string {
	const max = 2048
	s := string(out)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return strings.TrimSpace(s)
}
