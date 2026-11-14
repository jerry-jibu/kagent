package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/workspace"
)

// memBackend is an in-memory Backend for read-back tests: it promises the
// byte-faithful write/read round trip the Backend contract requires.
type memBackend struct {
	mu sync.Mutex
	files map[string]map[string][]byte // workspace -> path -> content
	// listedMissing are paths ListFiles reports but ReadFile refuses —
	// modeling a file that vanishes between listing and read-back.
	listedMissing map[string]map[string]bool // workspace -> path
}

func newMemBackend() *memBackend {
	return &memBackend{
		files:         map[string]map[string][]byte{},
		listedMissing: map[string]map[string]bool{},
	}
}

func (m *memBackend) EnsureWorkspace(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[name]; !ok {
		m.files[name] = map[string][]byte{}
	}
	return nil
}

func (m *memBackend) WriteFile(_ context.Context, ws, path string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	wsFiles, ok := m.files[ws]
	if !ok {
		return fmt.Errorf("workspace %s does not exist", ws)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("%w: %s escapes the workspace", workspace.ErrNotTransportable, path)
	}
	wsFiles[path] = append([]byte(nil), content...)
	return nil
}

func (m *memBackend) ReadFile(_ context.Context, ws, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wsFiles, ok := m.files[ws]
	if !ok {
		return nil, fmt.Errorf("workspace %s does not exist", ws)
	}
	content, ok := wsFiles[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", workspace.ErrFileNotFound, path)
	}
	return append([]byte(nil), content...), nil
}

func (m *memBackend) ListFiles(_ context.Context, ws, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wsFiles, ok := m.files[ws]
	if !ok {
		return nil, fmt.Errorf("workspace %s does not exist", ws)
	}
	var out []string
	for path := range wsFiles {
		if prefix == "" || strings.HasPrefix(path, prefix) {
			out = append(out, path)
		}
	}
	for path := range m.listedMissing[ws] {
		if prefix == "" || strings.HasPrefix(path, prefix) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out, nil
}

func TestReadWorkspaceState(t *testing.T) {
	t.Run("Given a workspace holding nested text files with unicode names, when the workspace state is read back, assert that every listed path resolves with byte-identical content", func(t *testing.T) {
		backend := newMemBackend()
		ctx := context.Background()
		const ws = "ticket-readback"
		if err := backend.EnsureWorkspace(ctx, ws); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		want := map[string][]byte{
			"README.md":              []byte("# hello\n"),
			"src/main.go":            []byte("package main\n"),
			"src/deep/pkg/util.go":   []byte("package pkg\n"),
			"docs/指南/中文.md":          []byte("中文内容\n"),
		}
		for path, content := range want {
			if err := backend.WriteFile(ctx, ws, path, content); err != nil {
				t.Fatalf("WriteFile %s failed: %v", path, err)
			}
		}

		state, err := workspace.ReadWorkspaceState(ctx, backend, ws)
		if err != nil {
			t.Fatalf("ReadWorkspaceState failed: %v", err)
		}
		if len(state) != len(want) {
			t.Fatalf("state holds %d files, want %d", len(state), len(want))
		}
		for path, content := range want {
			got, ok := state[path]
			if !ok {
				t.Fatalf("state is missing %s", path)
			}
			if string(got) != string(content) {
				t.Fatalf("content of %s does not round-trip byte-identical", path)
			}
		}
	})

	t.Run("Given an empty workspace, when the workspace state is read back, assert that the state is empty rather than an error — a ready report with an untouched workspace delivers an empty delta, not a crash", func(t *testing.T) {
		backend := newMemBackend()
		ctx := context.Background()
		const ws = "ticket-empty"
		if err := backend.EnsureWorkspace(ctx, ws); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}

		state, err := workspace.ReadWorkspaceState(ctx, backend, ws)
		if err != nil {
			t.Fatalf("ReadWorkspaceState failed: %v", err)
		}
		if len(state) != 0 {
			t.Fatalf("state = %v, want empty", state)
		}
	})

	t.Run("Given a file listed but deleted before its read-back, when the workspace state is read back, assert that the vanished path is treated as absent instead of failing the run", func(t *testing.T) {
		backend := newMemBackend()
		ctx := context.Background()
		const ws = "ticket-race"
		if err := backend.EnsureWorkspace(ctx, ws); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		if err := backend.WriteFile(ctx, ws, "keep.txt", []byte("kept\n")); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		// vanish.txt is listed but refuses reads: it disappeared after the
		// listing was taken (worker cleanup, checkpoint rollback).
		backend.listedMissing[ws] = map[string]bool{"vanish.txt": true}

		state, err := workspace.ReadWorkspaceState(ctx, backend, ws)
		if err != nil {
			t.Fatalf("ReadWorkspaceState failed: %v", err)
		}
		if _, ok := state["vanish.txt"]; ok {
			t.Fatalf("vanished path must not appear in the state")
		}
		if _, ok := state["keep.txt"]; !ok {
			t.Fatalf("kept path must appear in the state")
		}
	})

	t.Run("Given a backend outage, when the workspace state is read back, assert that the error surfaces for the caller to retry", func(t *testing.T) {
		backend := newMemBackend()
		ctx := context.Background()
		const ws = "absent-workspace"

		if _, err := workspace.ReadWorkspaceState(ctx, backend, ws); err == nil {
			t.Fatalf("reading an unknown workspace must fail")
		} else if errors.Is(err, workspace.ErrFileNotFound) {
			t.Fatalf("a backend outage is not a missing file: %v", err)
		}
	})
}

func TestAgentfsBackendListAndMissing(t *testing.T) {
	t.Run("Given the fake agentfs bridge holding a workspace with files, when the backend lists and reads, assert that the listing matches and a missing path reads as ErrFileNotFound", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx := context.Background()

		const ws = "ticket-list"
		if err := backend.EnsureWorkspace(ctx, ws); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}
		for path, content := range map[string]string{
			"README.md":    "# read\n",
			"src/agent.go": "package agent\n",
		} {
			if err := backend.WriteFile(ctx, ws, path, []byte(content)); err != nil {
				t.Fatalf("WriteFile %s failed: %v", path, err)
			}
		}

		listing, err := backend.ListFiles(ctx, ws, "")
		if err != nil {
			t.Fatalf("ListFiles failed: %v", err)
		}
		if len(listing) != 2 || listing[0] != "README.md" || listing[1] != "src/agent.go" {
			t.Fatalf("listing = %v, want the two written paths", listing)
		}

		if _, err := backend.ReadFile(ctx, ws, "src/missing.go"); !errors.Is(err, workspace.ErrFileNotFound) {
			t.Fatalf("missing path must read as ErrFileNotFound, got %v", err)
		}
	})

	t.Run("Given the fake agentfs bridge holding an empty workspace, when the backend lists it, assert that the listing is empty", func(t *testing.T) {
		_, backend := newTestAgentfsBackend(t)
		ctx := context.Background()
		const ws = "ticket-list-empty"
		if err := backend.EnsureWorkspace(ctx, ws); err != nil {
			t.Fatalf("EnsureWorkspace failed: %v", err)
		}

		listing, err := backend.ListFiles(ctx, ws, "")
		if err != nil {
			t.Fatalf("ListFiles failed: %v", err)
		}
		if len(listing) != 0 {
			t.Fatalf("listing = %v, want empty", listing)
		}
	})
}
