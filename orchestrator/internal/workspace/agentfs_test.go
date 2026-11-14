package workspace_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// deterministicText produces the i-th reproducible UTF-8 text payload of the
// given size. Expected values come from this independent generator, never
// from the implementation under test.
func deterministicText(i, size int) []byte {
	var b bytes.Buffer
	units := []string{
		"package main\n", "func main() {}\n", "// 中文注释内容\n",
		"const value = 0xdeadbeef\n", "emoji 🚀 line\n", "\tindented: true\n",
	}
	n := 0
	for b.Len() < size {
		unit := units[(i+n)%len(units)]
		b.WriteString(unit)
		if b.Len()%7 == 0 {
			b.WriteString(fmt.Sprintf("line %d of file %d\n", n, i))
		}
		n++
	}
	out := make([]byte, size)
	copy(out, b.Bytes())
	return out
}

func newTestAgentfsBackend(t *testing.T) (*fakeAgentfs, *workspace.AgentfsBackend) {
	t.Helper()
	fake := newFakeAgentfs()
	t.Cleanup(fake.close)
	backend := workspace.NewAgentfsBackend(fake.endpoint())
	return fake, backend
}

func TestAgentfsBackendWriteReadRoundTrip(t *testing.T) {
	t.Run("Given an empty fake agentfs bridge, when the backend creates a workspace, writes N files with varied content and reads them back by path, assert that every file returns byte-identical content", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const wsName = "ticket-0007"
		if err := backend.EnsureWorkspace(ctx, wsName); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}

		files := map[string][]byte{
			"empty.txt":                       {},
			"main.go":                         deterministicText(1, 512),
			"docs/中文说明.md":                    deterministicText(2, 1024),
			"pkg/deep/nested/child.go":        deterministicText(3, 4096),
			"pkg/deep/nested/deeper/leaf.txt": deterministicText(4, 256*1024),
		}
		for path, content := range files {
			if err := backend.WriteFile(ctx, wsName, path, content); err != nil {
				t.Fatalf("WriteFile %s failed: %v", path, err)
			}
		}
		for path, want := range files {
			got, err := backend.ReadFile(ctx, wsName, path)
			if err != nil {
				t.Fatalf("ReadFile %s failed: %v", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("ReadFile %s returned %d bytes, want byte-identical %d bytes", path, len(got), len(want))
			}
		}
	})

	t.Run("Given a workspace that already exists, when EnsureWorkspace runs again, assert that it is idempotent and does not fail", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := backend.EnsureWorkspace(ctx, "ticket-twice"); err != nil {
			t.Fatalf("first EnsureWorkspace failed: %v", err)
		}
		if err := backend.EnsureWorkspace(ctx, "ticket-twice"); err != nil {
			t.Fatalf("second EnsureWorkspace failed: %v", err)
		}
	})

	t.Run("Given binary content that the MCP string channel cannot carry losslessly, when WriteFile runs, assert that it refuses with an explanatory error instead of writing corrupted bytes", func(t *testing.T) {
		fake, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := backend.EnsureWorkspace(ctx, "ticket-bin"); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		binary := []byte{0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe, 0x00, 0x01}
		err := backend.WriteFile(ctx, "ticket-bin", "assets/logo.png", binary)
		if err == nil {
			t.Fatal("WriteFile accepted binary content, want refusal")
		}
		if !strings.Contains(err.Error(), "binary") {
			t.Fatalf("error = %v, want a binary-content explanation", err)
		}
		if _, present := fake.workspaceFiles("ticket-bin")["assets/logo.png"]; present {
			t.Fatal("corrupted bytes were written into the workspace")
		}
	})

	t.Run("Given content beyond the agentfs 4MiB read cap, when WriteFile runs, assert that it refuses with an explanatory error", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := backend.EnsureWorkspace(ctx, "ticket-big"); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		huge := bytes.Repeat([]byte("x"), (4<<20)+1)
		err := backend.WriteFile(ctx, "ticket-big", "big.bin.txt", huge)
		if err == nil {
			t.Fatal("WriteFile accepted over-cap content, want refusal")
		}
	})

	t.Run("Given a path escaping the workspace, when WriteFile runs, assert that it refuses", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := backend.EnsureWorkspace(ctx, "ticket-jail"); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		err := backend.WriteFile(ctx, "ticket-jail", "../escape.txt", []byte("nope"))
		if err == nil {
			t.Fatal("WriteFile accepted a traversal path, want refusal")
		}
	})

	t.Run("Given a path that was never written, when ReadFile runs, assert that it fails with a not-found error", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := backend.EnsureWorkspace(ctx, "ticket-miss"); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		_, err := backend.ReadFile(ctx, "ticket-miss", "no/such/file.txt")
		if err == nil {
			t.Fatal("ReadFile of a missing path succeeded, want error")
		}
	})
}
