package workspace_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// writeTree creates files under dir with the given relative path -> content
// map, creating parent directories.
func writeTree(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestMaterializeDirectory(t *testing.T) {
	t.Run("Given a local directory tree with nested text files, a binary file and a symlink, when MaterializeDirectory runs against the fake agentfs backend, assert that every text file lands byte-identical, untransportable paths are reported as skipped and counts are exact", func(t *testing.T) {
		fake, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		tree := map[string][]byte{
			"README.md":           deterministicText(1, 512),
			"src/main.go":         deterministicText(2, 1024),
			"src/internal/dep.go": deterministicText(3, 2048),
			"docs/guide/中文.md":    deterministicText(4, 512),
			"assets/logo.png":     {0x89, 0x50, 0x4e, 0x47, 0xff, 0x00},
		}
		root := t.TempDir()
		writeTree(t, root, tree)
		if err := os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "LINK.md")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		const wsName = "ticket-tree"
		res, err := workspace.MaterializeDirectory(ctx, backend, wsName, root, workspace.MaterializeOptions{})
		if err != nil {
			t.Fatalf("MaterializeDirectory failed: %v", err)
		}

		if res.Workspace != wsName {
			t.Fatalf("result workspace = %q, want %q", res.Workspace, wsName)
		}
		// Files counts what landed through the backend; the binary file is
		// reported skipped, not written.
		if res.Files != len(tree)-1 {
			t.Fatalf("result files = %d, want %d (tree minus the skipped binary file)", res.Files, len(tree)-1)
		}
		var wantBytes int64
		for rel, c := range tree {
			if rel == "assets/logo.png" {
				continue // skipped, not written
			}
			wantBytes += int64(len(c))
		}
		if res.Bytes != wantBytes {
			t.Fatalf("result bytes = %d, want %d", res.Bytes, wantBytes)
		}

		skipped := map[string]string{}
		for _, s := range res.Skipped {
			skipped[s.Path] = s.Reason
		}
		if _, ok := skipped["assets/logo.png"]; !ok {
			t.Fatalf("binary file not reported skipped: %+v", res.Skipped)
		}
		if _, ok := skipped["LINK.md"]; !ok {
			t.Fatalf("symlink not reported skipped: %+v", res.Skipped)
		}
		if len(res.Skipped) != 2 {
			t.Fatalf("skipped = %+v, want exactly the binary file and the symlink", res.Skipped)
		}

		// Byte-identity on read-back, per the materializer contract.
		for rel, want := range tree {
			if _, isSkipped := skipped[rel]; isSkipped {
				continue
			}
			got, err := backend.ReadFile(ctx, wsName, rel)
			if err != nil {
				t.Fatalf("ReadFile %s failed: %v", rel, err)
			}
			if !bytesEqual(got, want) {
				t.Fatalf("read-back of %s is not byte-identical", rel)
			}
		}

		names := fake.callNames()
		if len(names) == 0 {
			t.Fatal("no tool calls recorded; materialization never reached the bridge")
		}
	})

	t.Run("Given a cloned-tree layout that contains .git, when MaterializeDirectory runs with default options, assert that .git is not materialized and its object files are not even attempted", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		root := t.TempDir()
		writeTree(t, root, map[string][]byte{
			"main.go":                      []byte("package main\n"),
			".git/config":                  []byte("[remote \"origin\"]\n\turl = https://github.com/o/r.git\n"),
			".git/objects/pack/pack.index": {0x00, 0x01, 0x02, 0xff},
		})

		res, err := workspace.MaterializeDirectory(ctx, backend, "ticket-git", root, workspace.MaterializeOptions{})
		if err != nil {
			t.Fatalf("MaterializeDirectory failed: %v", err)
		}
		if res.Files != 1 {
			t.Fatalf("result files = %d, want exactly the one non-git file", res.Files)
		}
		for _, s := range res.Skipped {
			if filepath.IsAbs(s.Path) || filepath.Base(filepath.Dir(s.Path)) == ".git" || s.Path == ".git/config" || s.Path == ".git/objects/pack/pack.index" {
				t.Fatalf(".git path leaked into materialization: %s", s.Path)
			}
		}
	})
}

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
