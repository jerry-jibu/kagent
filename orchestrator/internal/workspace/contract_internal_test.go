package workspace

import (
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
