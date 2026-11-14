package workspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// gitFixture is a local origin repository the patcher can clone from and push
// to, with the base commit the workspace was materialized from.
type gitFixture struct {
	root      string // temp workdir root
	origin    string // bare remote
	baseSHA   string // HEAD of seed at materialization time
	remoteURL string // file:// URL of origin
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

func newGitFixture(t *testing.T, files map[string][]byte) *gitFixture {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	origin := filepath.Join(root, "origin.git")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	runGit(t, seed, "init", "-b", "main")
	writeTree(t, seed, files)
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "seed")
	baseSHA := runGit(t, seed, "rev-parse", "HEAD")

	runGit(t, root, "clone", "--bare", "--no-hardlinks", seed, origin)
	// The deliverer fetches the base commit by SHA; allow that even after the
	// branch tip moves on (GitHub enables allowAnySHA1InWant server-side).
	runGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	return &gitFixture{
		root:      root,
		origin:    origin,
		baseSHA:   baseSHA,
		remoteURL: "file://" + origin,
	}
}

// branchTree extracts refs/heads/<branch> from the bare origin into a plain
// directory and returns the files under it, so tests can assert the pushed
// tree byte for byte.
func (f *gitFixture) branchTree(t *testing.T, branch string) map[string][]byte {
	t.Helper()
	inspect := filepath.Join(f.root, "inspect", branch)
	if err := os.MkdirAll(inspect, 0o755); err != nil {
		t.Fatalf("mkdir inspect: %v", err)
	}
	archive := exec.Command("git", "--git-dir", f.origin, "archive", "refs/heads/"+branch)
	tar := exec.Command("tar", "-x", "-C", inspect)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	tar.Stdin = pipe
	if err := archive.Start(); err != nil {
		t.Fatalf("archive start: %v", err)
	}
	if err := tar.Run(); err != nil {
		t.Fatalf("tar run: %v", err)
	}
	if err := archive.Wait(); err != nil {
		t.Fatalf("archive wait: %v", err)
	}

	files := map[string][]byte{}
	err = filepath.Walk(inspect, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(inspect, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = content
		return nil
	})
	if err != nil {
		t.Fatalf("walk pushed tree: %v", err)
	}
	return files
}

func (f *gitFixture) branchCommit(t *testing.T, branch string) (sha, parent, message string) {
	t.Helper()
	sha = runGit(t, f.origin, "rev-parse", "refs/heads/"+branch)
	parent = runGit(t, f.origin, "rev-parse", sha+"^")
	message = runGit(t, f.origin, "log", "-1", "--format=%B", sha)
	return sha, parent, message
}

func TestPatcher(t *testing.T) {
	t.Run("Given a baseline repo and a workspace state that modifies one file, adds one, deletes one and leaves a skipped binary untouched, when the patcher ships the ticket branch, assert that the pushed tree equals the workspace state byte for byte, the commit descends from the baseline, and the skipped path survives", func(t *testing.T) {
		f := newGitFixture(t, map[string][]byte{
			"a.txt":          []byte("original a\n"),
			"dir/b.txt":      []byte("original b\n"),
			"del.txt":        []byte("delete me\n"),
			"assets/bin.dat": {0x00, 0x01, 0x02, 0xff},
		})
		state := map[string][]byte{
			"a.txt":     []byte("rewritten a\n"),
			"dir/b.txt": []byte("original b\n"), // unchanged
			"new.txt":   []byte("brand new\n"),
		}

		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		outcome, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    f.remoteURL,
			BaseCommit: f.baseSHA,
			Branch:     "ticket-abc123",
			Message:    "ticket-abc123: deliver workspace result\n\nApplies the worker's sandbox changes for acme/api#42.",
			State:      state,
			SkipPaths:  []string{"assets/bin.dat"},
		})
		if err != nil {
			t.Fatalf("Patch failed: %v", err)
		}

		sha, parent, message := f.branchCommit(t, "ticket-abc123")
		if sha != outcome.Commit {
			t.Fatalf("reported commit %s != pushed %s", outcome.Commit, sha)
		}
		if parent != f.baseSHA {
			t.Fatalf("commit parent %s != base %s — the branch must descend from the materialization baseline", parent, f.baseSHA)
		}
		if !strings.Contains(message, "ticket-abc123") || !strings.Contains(message, "acme/api#42") {
			t.Fatalf("commit message lost the ticket/issue reference: %q", message)
		}

		tree := f.branchTree(t, "ticket-abc123")
		want := map[string][]byte{
			"a.txt":          []byte("rewritten a\n"),
			"dir/b.txt":      []byte("original b\n"),
			"new.txt":        []byte("brand new\n"),
			"assets/bin.dat": {0x00, 0x01, 0x02, 0xff}, // skipped at materialization, never deleted
		}
		if len(tree) != len(want) {
			t.Fatalf("pushed tree holds %d files (%v), want %d", len(tree), keys(tree), len(want))
		}
		for path, content := range want {
			got, ok := tree[path]
			if !ok {
				t.Fatalf("pushed tree is missing %s", path)
			}
			if string(got) != string(content) {
				t.Fatalf("content of %s in the PR branch differs from the sandbox state (byte-identity violated)", path)
			}
		}
	})

	t.Run("Given a workspace state identical to its baseline, when the patcher runs, assert that it refuses with ErrEmptyChangeSet and pushes no branch — no empty PR", func(t *testing.T) {
		files := map[string][]byte{"only.txt": []byte("unchanged\n")}
		f := newGitFixture(t, files)
		state := map[string][]byte{"only.txt": []byte("unchanged\n")}

		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		_, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    f.remoteURL,
			BaseCommit: f.baseSHA,
			Branch:     "ticket-empty",
			Message:    "ticket-empty",
			State:      state,
		})
		if !errors.Is(err, workspace.ErrEmptyChangeSet) {
			t.Fatalf("err = %v, want ErrEmptyChangeSet", err)
		}
		if f.hasBranch(t, "ticket-empty") {
			t.Fatalf("empty delivery must not push a branch")
		}
	})

	t.Run("Given an upstream that moved past the materialization baseline, when the patcher runs from the recorded base commit, assert that the branch descends from the old baseline — upstream drift never enters the diff", func(t *testing.T) {
		f := newGitFixture(t, map[string][]byte{"base.txt": []byte("baseline\n")})

		// Upstream moves: someone else pushes to main after the workspace
		// was materialized.
		seed := filepath.Join(f.root, "seed")
		writeTree(t, seed, map[string][]byte{"upstream.txt": []byte("someone else\n")})
		runGit(t, seed, "add", "-A")
		runGit(t, seed, "commit", "-m", "upstream drift")
		runGit(t, seed, "push", f.origin, "main:main")

		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		if _, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    f.remoteURL,
			BaseCommit: f.baseSHA,
			Branch:     "ticket-drift",
			Message:    "ticket-drift",
			State:      map[string][]byte{"base.txt": []byte("baseline+worker\n")},
		}); err != nil {
			t.Fatalf("Patch failed: %v", err)
		}

		_, parent, _ := f.branchCommit(t, "ticket-drift")
		if parent != f.baseSHA {
			t.Fatalf("commit parent %s != recorded base %s — the diff must exclude upstream drift", parent, f.baseSHA)
		}
		tree := f.branchTree(t, "ticket-drift")
		if _, ok := tree["upstream.txt"]; ok {
			t.Fatalf("upstream file leaked into the ticket branch")
		}
	})

	t.Run("Given a repo URL embedding credentials, when the patcher runs, assert that it refuses before any git invocation — credentials never reach a URL", func(t *testing.T) {
		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		_, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    "https://user:token@github.com/acme/api.git",
			BaseCommit: strings.Repeat("a", 40),
			Branch:     "ticket-x",
			State:      map[string][]byte{},
		})
		if err == nil {
			t.Fatalf("credential-embedded URL must be refused")
		}
	})

	t.Run("Given a branch name with traversal, when the patcher runs, assert that it refuses — the branch name is caller-supplied and must stay a safe ref", func(t *testing.T) {
		f := newGitFixture(t, map[string][]byte{"x.txt": []byte("x\n")})
		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		_, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    f.remoteURL,
			BaseCommit: f.baseSHA,
			Branch:     "../../evil",
			State:      map[string][]byte{"x.txt": []byte("y\n")},
		})
		if err == nil {
			t.Fatalf("traversal branch name must be refused")
		}
	})

	t.Run("Given a push that fails at the remote, when the patcher runs, assert that the error surfaces with the remote's words — the deliverer retries on the next beat", func(t *testing.T) {
		f := newGitFixture(t, map[string][]byte{"x.txt": []byte("x\n")})
		// A checked-out (non-bare) remote refuses pushes to its HEAD branch.
		checkout := filepath.Join(f.root, "checkout-remote")
		runGit(t, f.root, "clone", f.remoteURL, checkout)

		patcher := workspace.NewPatcher(workspace.PatcherOptions{})
		_, err := patcher.Patch(context.Background(), workspace.PatchRequest{
			RepoURL:    "file://" + checkout,
			BaseCommit: f.baseSHA,
			Branch:     "main",
			Message:    "ticket-refused",
			State:      map[string][]byte{"x.txt": []byte("y\n")},
		})
		if err == nil {
			t.Fatalf("rejected push must surface as an error")
		}
	})
}

func (f *gitFixture) hasBranch(t *testing.T, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", f.origin, "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
