package policy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
)

// writePolicyDir materializes a policy directory the way a mounted ConfigMap
// looks on disk: three JSON files (repos, admission, bots).
func writePolicyDir(t *testing.T, repos, admission, bots string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"repos.json":     repos,
		"admission.json": admission,
		"bots.json":      bots,
	}
	for name, content := range files {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}
	return dir
}

func validRepos() string {
	return `{"repositories": ["acme/widgets", "acme/gadgets"]}`
}

func validAdmission() string {
	return `{"senders": [
		{"id": "alice", "strength": "explicit"},
		{"id": "bob",   "strength": "inferred"}
	]}`
}

func validBots() string {
	return `{"bots": ["acme-bot[bot]"]}`
}

func TestLoad(t *testing.T) {
	t.Run("Given a policy directory with valid repos, admission and bots files, when loaded, assert that the policy parses and the version hash is a 64-character sha256 that is stable across loads", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())

		first, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("load failed: %v", err)
		}
		if len(first.Repositories) != 2 || first.Repositories[0] != "acme/widgets" {
			t.Fatalf("unexpected repositories: %v", first.Repositories)
		}
		if len(first.Admission) != 2 {
			t.Fatalf("unexpected admission entries: %+v", first.Admission)
		}
		if len(first.Bots) != 1 {
			t.Fatalf("unexpected bots: %v", first.Bots)
		}
		if len(first.Version) != 64 {
			t.Fatalf("version hash must be 64 hex chars, got %q", first.Version)
		}

		second, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("second load failed: %v", err)
		}
		if second.Version != first.Version {
			t.Fatalf("hash must be deterministic: %s vs %s", first.Version, second.Version)
		}
	})

	t.Run("Given the same policy content with different JSON formatting, when loaded, assert that the version hash is unchanged", func(t *testing.T) {
		compact := writePolicyDir(t, `{"repositories":["acme/widgets"]}`, `{"senders":[{"id":"alice","strength":"explicit"}]}`, `{"bots":[]}`)
		pretty := writePolicyDir(t, "{\n  \"repositories\": [\n    \"acme/widgets\"\n  ]\n}", "{\n  \"senders\": [{\"id\": \"alice\", \"strength\": \"explicit\"}]\n}", "{\n  \"bots\": []\n}")

		a, err := policy.Load(compact)
		if err != nil {
			t.Fatalf("compact load failed: %v", err)
		}
		b, err := policy.Load(pretty)
		if err != nil {
			t.Fatalf("pretty load failed: %v", err)
		}
		if a.Version != b.Version {
			t.Fatalf("whitespace-only differences must not change the policy version: %s vs %s", a.Version, b.Version)
		}
	})

	t.Run("Given a policy directory whose repos.json is malformed JSON, when loaded, assert that the load fails and the error names the offending file", func(t *testing.T) {
		dir := writePolicyDir(t, `{"repositories": [`, validAdmission(), validBots())

		_, err := policy.Load(dir)
		if err == nil {
			t.Fatal("malformed repos.json must fail the load")
		}
		if !strings.Contains(err.Error(), "repos.json") {
			t.Fatalf("error must name the offending file, got: %v", err)
		}
	})

	t.Run("Given an admission entry with an unknown strength, when loaded, assert that the load fails", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), `{"senders": [{"id": "alice", "strength": "yolo"}]}`, validBots())

		_, err := policy.Load(dir)
		if err == nil {
			t.Fatal("unknown admission strength must fail the load")
		}
		if !strings.Contains(err.Error(), "yolo") {
			t.Fatalf("error must name the offending value, got: %v", err)
		}
	})

	t.Run("Given a repository entry that is not owner/name, when loaded, assert that the load fails", func(t *testing.T) {
		dir := writePolicyDir(t, `{"repositories": ["just-a-name"]}`, validAdmission(), validBots())

		if _, err := policy.Load(dir); err == nil {
			t.Fatal("malformed repository entry must fail the load")
		}
	})

	t.Run("Given a policy directory missing the bots file, when loaded, assert that the load fails and names the missing file", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), "")

		_, err := policy.Load(dir)
		if err == nil {
			t.Fatal("missing bots.json must fail the load")
		}
		if !strings.Contains(err.Error(), "bots.json") {
			t.Fatalf("error must name the missing file, got: %v", err)
		}
	})

	t.Run("Given a changed repository list, when reloaded, assert that the version hash differs from the previous policy", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())
		before, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("initial load failed: %v", err)
		}

		if err := os.WriteFile(filepath.Join(dir, "repos.json"), []byte(`{"repositories": ["acme/widgets", "acme/gadgets", "acme/extra"]}`), 0o644); err != nil {
			t.Fatalf("failed to rewrite repos.json: %v", err)
		}

		after, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("reload failed: %v", err)
		}
		if after.Version == before.Version {
			t.Fatal("changed content must produce a new version hash")
		}
	})
}

func TestCanonical(t *testing.T) {
	t.Run("Given a loaded policy, when its canonical bytes are hashed, assert that the result equals the policy version", func(t *testing.T) {
		dir := writePolicyDir(t, validRepos(), validAdmission(), validBots())
		p, err := policy.Load(dir)
		if err != nil {
			t.Fatalf("load failed: %v", err)
		}

		sum := sha256.Sum256(p.Canonical())
		if got := hex.EncodeToString(sum[:]); got != p.Version {
			t.Fatalf("canonical bytes must hash to Version: got %s, want %s", got, p.Version)
		}
	})
}
