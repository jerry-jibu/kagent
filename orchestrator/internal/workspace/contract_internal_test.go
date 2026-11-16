package workspace

import (
	"errors"
	"strings"
	"testing"
)

// Given untrusted workspace state paths, when they are validated, assert that
// gitdir access is refused at every depth and case while lookalike paths stay
// legal — a state key reaching the baseline clone's .git would execute with
// the orchestrator's credentials on the next git commit (hooks).
func TestValidateWorkspacePathGitdir(t *testing.T) {
	t.Run("Given state keys that target the git directory, when validated, assert each is refused", func(t *testing.T) {
		for _, path := range []string{
			".git/config",
			".git/hooks/pre-commit",
			".GIT/config",
			".Git/hooks/pre-commit",
			"pkg/sub/.git/objects/pack/x.pack", // nested, not just the root
		} {
			err := validateWorkspacePath(path)
			if err == nil {
				t.Fatalf("path %q must be refused as gitdir access", path)
			}
			if !strings.Contains(err.Error(), "git directory") {
				t.Fatalf("path %q refusal must name the git directory, got: %v", path, err)
			}
		}
	})
	t.Run("Given state keys that merely resemble the git directory, when validated, assert they stay legal", func(t *testing.T) {
		for _, path := range []string{
			".github/workflows/ci.yml",
			".gitignore",
			".gitattributes",
			"pkg/gitdocs/readme.md",
			"docs/.git-history-notes.md",
		} {
			if err := validateWorkspacePath(path); err != nil {
				t.Fatalf("path %q must stay legal, got: %v", path, err)
			}
		}
	})
}

// Given read_file refusals of every shape the error chain can carry, when
// absence is classified, assert that ONLY the engine's exact
// "file not found: <path>" format naming the requested path reads as absence
// — absence feeds the patcher's deletions of baseline files, so any looser
// match (generic wording, another path's name) must classify as an outage.
func TestIsAgentfsFileNotFound(t *testing.T) {
	const path = "docs/指南.md"
	t.Run("Given the engine's exact absence format with the matching path, when classified, assert it reads as absence — including behind the wire wrapper prefixes", func(t *testing.T) {
		for _, msg := range []string{
			"file not found: docs/指南.md",
			"tool agenticfs_read_file reported an error: file not found: docs/指南.md",
			"failed to read docs/指南.md: tool agenticfs_read_file reported an error: file not found: docs/指南.md",
			"JSON-RPC error -32000: file not found: docs/指南.md",
		} {
			if !isAgentfsFileNotFound(errors.New(msg), path) {
				t.Fatalf("message %q must classify as absence for path %q", msg, path)
			}
		}
	})
	t.Run("Given generic missing-file wording with no path in it, when classified, assert it reads as an outage — a workspace-level failure mid-read-back must never masquerade as absence", func(t *testing.T) {
		for _, msg := range []string{
			"file not found",
			"no such file or directory",
			"workspace ticket-1 does not exist",
			"tool agenticfs_read_file reported an error: engine store does not exist",
		} {
			if isAgentfsFileNotFound(errors.New(msg), path) {
				t.Fatalf("message %q must classify as an outage, not absence", msg)
			}
		}
	})
	t.Run("Given the absence format naming a different path than requested, when classified, assert it reads as an outage", func(t *testing.T) {
		for _, msg := range []string{
			"file not found: other/README.md",
			"tool agenticfs_read_file reported an error: file not found: docs/指南.md.bak",
		} {
			if isAgentfsFileNotFound(errors.New(msg), path) {
				t.Fatalf("message %q names another path; it must classify as an outage, not absence for %q", msg, path)
			}
		}
	})
	t.Run("Given a nil error, when classified, assert absence is never reported", func(t *testing.T) {
		if isAgentfsFileNotFound(nil, path) {
			t.Fatal("a nil error must never classify as absence")
		}
	})
}
