package policy_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
)

// waitFor polls cond until it holds or the deadline passes; the test fails
// at the deadline with last observed state in the message.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startTestWatcher starts a watcher with a fast poll interval over dir and
// stops it when the test ends.
func startTestWatcher(t *testing.T, dir string, onUpdate func(policy.Policy) error) *policy.Watcher {
	t.Helper()
	w, err := policy.StartWatcher(dir, 10*time.Millisecond, onUpdate, testLogger())
	if err != nil {
		t.Fatalf("failed to start watcher: %v", err)
	}
	t.Cleanup(w.Stop)
	return w
}

func testLogger() *slog.Logger { return slog.Default() }

func TestWatcher(t *testing.T) {
	t.Run("Given a running watcher over a valid policy directory, when a file changes on disk, assert that Current reports the new version hash and the update callback receives the new policy", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())
		before, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("initial load failed: %v", err)
		}

		var mu sync.Mutex
		var updates []policy.Policy
		w := startTestWatcher(t, dir, func(p policy.Policy) error {
			mu.Lock()
			defer mu.Unlock()
			updates = append(updates, p)
			return nil
		})
		if w.Current().Version != before.Version {
			t.Fatalf("initial policy version mismatch: %s vs %s", w.Current().Version, before.Version)
		}

		if err := os.WriteFile(filepath.Join(dir, "repos.json"), []byte(`{"repositories": ["acme/widgets", "acme/next"]}`), 0o644); err != nil {
			t.Fatalf("failed to rewrite repos.json: %v", err)
		}

		want, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("reload failed: %v", err)
		}
		waitFor(t, "watcher to observe the new policy version", func() bool {
			return w.Current().Version == want.Version
		})
		mu.Lock()
		defer mu.Unlock()
		if len(updates) == 0 || updates[len(updates)-1].Version != want.Version {
			t.Fatalf("update callback must deliver the new policy, got %+v", updates)
		}
	})

	t.Run("Given a watcher serving a good policy, when a file becomes malformed, assert that Current keeps returning the last good policy and no update is delivered", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())
		good, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("initial load failed: %v", err)
		}

		var mu sync.Mutex
		updates := 0
		w := startTestWatcher(t, dir, func(p policy.Policy) error {
			mu.Lock()
			defer mu.Unlock()
			updates++
			return nil
		})

		if err := os.WriteFile(filepath.Join(dir, "bots.json"), []byte(`{"bots": [`), 0o644); err != nil {
			t.Fatalf("failed to corrupt bots.json: %v", err)
		}

		// Give the watcher plenty of ticks to observe the corruption.
		time.Sleep(200 * time.Millisecond)
		if w.Current().Version != good.Version {
			t.Fatalf("broken reload must not change the served policy: %s vs %s", w.Current().Version, good.Version)
		}
		mu.Lock()
		defer mu.Unlock()
		if updates != 0 {
			t.Fatalf("no update may be delivered for a failed reload, got %d", updates)
		}
	})

	t.Run("Given an update callback that fails transiently, when the callback recovers, assert that the watcher retries so the version is eventually delivered", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())

		var mu sync.Mutex
		var delivered []string
		w := startTestWatcher(t, dir, func(p policy.Policy) error {
			mu.Lock()
			defer mu.Unlock()
			delivered = append(delivered, p.Version)
			return nil
		})
		if w.Current().Version == "" {
			t.Fatal("watcher must serve a policy immediately after start")
		}

		if err := os.WriteFile(filepath.Join(dir, "repos.json"), []byte(`{"repositories": ["acme/only"]}`), 0o644); err != nil {
			t.Fatalf("failed to rewrite repos.json: %v", err)
		}
		want, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("reload failed: %v", err)
		}
		waitFor(t, "policy version to be delivered to the callback", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(delivered) > 0 && delivered[len(delivered)-1] == want.Version
		})
	})
}
