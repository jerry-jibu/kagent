package workspace_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// v6FileCount and v6FileBytes size the synthetic repo for the V6 scenario
// (docs/designs/0003 V6): a mid-scale repository — well under the 500MiB
// bound — whose clone+materialize must complete inside the 60s budget.
const (
	v6FileCount = 2000
	v6FileBytes = 24 * 1024
)

// buildV6Repo creates a local git repository with v6FileCount deterministic
// text files and commits them on main.
func buildV6Repo(t *testing.T) (string, int64) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	var total int64
	for i := 0; i < v6FileCount; i++ {
		dir := filepath.Join(repo, fmt.Sprintf("pkg%02d", i%40))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		for b.Len() < v6FileBytes {
			fmt.Fprintf(&b, "// file %d line %d: orchestrator workspace materialization scale probe\n", i, b.Len())
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.go", i)), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		total += int64(b.Len())
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "-c", "user.email=orc@test", "-c", "user.name=orc", "commit", "-q", "-m", "v6 scale fixture")
	return repo, total
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("size walk failed: %v", err)
	}
	return total
}

func gitVersionOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH; skipping V6 scale scenario")
	}
}

// TestMaterializeTicketWorkspace_V6Scale runs the docs/designs/0003 V6
// scenario against the fake agentfs bridge: mid-scale repo (<500MiB),
// clone+materialize <60s. It exercises the orchestrator-side pipeline
// (clone → walk → per-file MCP calls); real-bridge timing lands with the
// in-cluster integration (T9).
func TestMaterializeTicketWorkspace_V6Scale(t *testing.T) {
	gitVersionOrSkip(t)
	if testing.Short() {
		t.Skip("short mode: skipping V6 scale scenario")
	}

	repo, sourceBytes := buildV6Repo(t)
	_, fake, m := newFlowFixture(t, "ticket-v6", "main", "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cloneStart := time.Now()
	res, err := m.MaterializeTicketWorkspace(ctx, "ticket-v6", workspace.CloneRequest{
		RepoURL: fileURL(repo),
		Branch:  "main",
	})
	elapsed := time.Since(cloneStart)
	if err != nil {
		t.Fatalf("V6 materialization failed: %v", err)
	}

	if res.Files != v6FileCount {
		t.Fatalf("materialized files = %d, want %d", res.Files, v6FileCount)
	}
	if res.Bytes != sourceBytes {
		t.Fatalf("materialized bytes = %d, want %d", res.Bytes, sourceBytes)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none for an all-text tree", res.Skipped)
	}

	repoSize := dirSize(t, repo)
	if repoSize > 500<<20 {
		t.Fatalf("fixture repo is %d bytes, the V6 scenario is bound to <500MiB", repoSize)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("clone+materialize took %s, want <60s (V6 budget)", elapsed)
	}

	// Spot-check byte-identity at scale, per the materializer contract.
	backend := workspace.NewAgentfsBackend(fake.endpoint())
	for _, rel := range []string{"pkg00/file0000.go", "pkg19/file0019.go", fmt.Sprintf("pkg%02d/file%04d.go", (v6FileCount-1)%40, v6FileCount-1)} {
		got, err := backend.ReadFile(ctx, "ticket-v6", rel)
		if err != nil {
			t.Fatalf("spot read-back of %s failed: %v", rel, err)
		}
		want, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("spot read-back of %s is not byte-identical", rel)
		}
	}

	t.Logf("V6: repo %.1fMiB, %d files, clone+materialize %s (budget 60s)",
		float64(repoSize)/(1<<20), res.Files, elapsed.Round(time.Millisecond))
}
