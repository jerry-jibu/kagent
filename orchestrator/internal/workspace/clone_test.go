package workspace_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

const testToken = "github_pat_11AAAAAAA0_supersecret_T5_token"

// gitRun runs git in dir and fails the test on error, returning output.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// newSourceRepo builds a local git repository with a main branch and
// returns its root and its file map (relative path -> content).
func newSourceRepo(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	files := map[string][]byte{
		"README.md":   []byte("# demo repo\n"),
		"src/main.go": []byte("package main\n\nfunc main() {}\n"),
	}
	for rel, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "-c", "user.email=orc@test", "-c", "user.name=orc", "commit", "-m", "initial", "-q")
	return repo, files
}

func fileURL(repo string) string { return "file://" + repo }

func TestClonerClone(t *testing.T) {
	t.Run("Given a local repository with branch main, when the cloner shallow-clones that branch, assert that the working copy contains the branch files, the base commit matches the branch HEAD and the clone is shallow", func(t *testing.T) {
		repo, files := newSourceRepo(t)
		head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "main"))

		cloner := workspace.NewCloner(workspace.ClonerOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		out, err := cloner.Clone(ctx, workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "main",
		})
		if err != nil {
			t.Fatalf("Clone failed: %v", err)
		}
		defer os.RemoveAll(out.Dir)

		if out.BaseCommit != head {
			t.Fatalf("BaseCommit = %s, want branch HEAD %s", out.BaseCommit, head)
		}
		for rel, want := range files {
			got, err := os.ReadFile(filepath.Join(out.Dir, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("cloned working copy misses %s: %v", rel, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("cloned %s content differs from the source branch", rel)
			}
		}
		if _, err := os.Stat(filepath.Join(out.Dir, ".git", "shallow")); err != nil {
			t.Fatal("clone is not shallow (.git/shallow missing)")
		}
	})

	t.Run("Given a clone request with a token, when the clone completes, assert that the token appears nowhere on disk inside the clone and never in the git argv", func(t *testing.T) {
		repo, _ := newSourceRepo(t)

		cloner := workspace.NewCloner(workspace.ClonerOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		out, err := cloner.Clone(ctx, workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "main",
			Token:   testToken,
		})
		if err != nil {
			t.Fatalf("Clone failed: %v", err)
		}
		defer os.RemoveAll(out.Dir)

		argv := workspace.BuildCloneArgs(fileURL(repo), "main", out.Dir)
		if strings.Contains(strings.Join(argv, " "), testToken) {
			t.Fatal("token leaked into the git argv")
		}

		err = filepath.WalkDir(out.Dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(content), testToken) {
				t.Errorf("token found on disk in %s", p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("token sweep failed: %v", err)
		}
	})

	t.Run("Given a branch that does not exist upstream, when the cloner runs, assert that it fails promptly with an error naming the branch and never hangs", func(t *testing.T) {
		repo, _ := newSourceRepo(t)

		cloner := workspace.NewCloner(workspace.ClonerOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		start := time.Now()
		_, err := cloner.Clone(ctx, workspace.CloneRequest{
			RepoURL: fileURL(repo),
			Branch:  "no-such-branch",
		})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Clone of a nonexistent branch succeeded, want failure")
		}
		if !strings.Contains(err.Error(), "no-such-branch") {
			t.Fatalf("error = %v, want it to name the missing branch", err)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("Clone took %s, want prompt failure (不挂起)", elapsed)
		}
	})

	t.Run("Given a repository path that does not exist, when the cloner runs, assert that it fails with an error", func(t *testing.T) {
		cloner := workspace.NewCloner(workspace.ClonerOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		_, err := cloner.Clone(ctx, workspace.CloneRequest{
			RepoURL: fileURL(filepath.Join(t.TempDir(), "missing-repo")),
			Branch:  "main",
		})
		if err == nil {
			t.Fatal("Clone of a missing repository succeeded, want failure")
		}
	})

	t.Run("Given a repo URL with credentials embedded, when the cloner runs, assert that it refuses before any git invocation", func(t *testing.T) {
		cloner := workspace.NewCloner(workspace.ClonerOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := cloner.Clone(ctx, workspace.CloneRequest{
			RepoURL: "https://user:tok@example.com/o/r.git",
			Branch:  "main",
			Token:   testToken,
		})
		if err == nil {
			t.Fatal("Clone accepted a URL with embedded credentials, want refusal")
		}
	})
}

func TestBuildCloneArgs(t *testing.T) {
	t.Run("Given a repo URL and branch, when clone args are built, assert that the command is a depth-1 single-branch clone and carries no credential material", func(t *testing.T) {
		argv := workspace.BuildCloneArgs("https://github.com/o/r.git", "feature/x", "/tmp/dest")
		joined := strings.Join(argv, " ")
		for _, want := range []string{
			"--depth 1",
			"--single-branch",
			"--branch feature/x",
			"https://github.com/o/r.git",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("argv %q misses %q", joined, want)
			}
		}
		// The credential helper must reference the token by environment
		// variable, never by value.
		if !strings.Contains(joined, "password=$ORCHESTRATOR_GIT_TOKEN") {
			t.Fatalf("argv lacks the env-var credential helper: %q", joined)
		}
		if strings.Contains(joined, "password=x-access-token:tok") || strings.Contains(joined, testToken) {
			t.Fatalf("argv carries credential material: %q", joined)
		}
	})
}

func TestValidateRepoURL(t *testing.T) {
	t.Run("Given URLs with embedded userinfo, when validated, assert that each is refused", func(t *testing.T) {
		for _, bad := range []string{
			"https://user:tok@example.com/o/r.git",
			"https://tok@github.com/o/r.git",
			"http://user:pass@host/repo.git",
		} {
			if err := workspace.ValidateRepoURL(bad); err == nil {
				t.Errorf("ValidateRepoURL(%q) accepted embedded credentials", bad)
			}
		}
	})

	t.Run("Given a clean https URL, when validated, assert that it is accepted", func(t *testing.T) {
		if err := workspace.ValidateRepoURL("https://github.com/o/r.git"); err != nil {
			t.Fatalf("ValidateRepoURL rejected a clean URL: %v", err)
		}
	})
}
