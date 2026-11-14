package intake_test

import (
	"testing"

	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
)

// Story 27: an issue can point the work at a branch other than the
// deployment default. The deterministic contract is a `base-branch: <name>`
// line in the trigger's comment body; a body without one yields "" and the
// caller applies the deployment-level default (--workspace-base-branch).
func TestBaseBranchFromBody(t *testing.T) {
	t.Run("Given a comment body carrying a base-branch line, when parsed, assert that the named branch is returned", func(t *testing.T) {
		body := "@acme-bot please fix the login test\nbase-branch: fix/login\nthanks"
		if got := intake.BaseBranchFromBody(body); got != "fix/login" {
			t.Fatalf("base branch = %q, want fix/login", got)
		}
	})

	t.Run("Given leading whitespace before the marker and around the value, when parsed, assert that the branch is trimmed", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("\n  base-branch:   release-1.2  \n"); got != "release-1.2" {
			t.Fatalf("base branch = %q, want release-1.2", got)
		}
	})

	t.Run("Given a body without a base-branch line, when parsed, assert that the result is empty (caller applies the deployment default)", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("@acme-bot please fix the login test"); got != "" {
			t.Fatalf("base branch = %q, want empty", got)
		}
	})

	t.Run("Given a marker with no branch value, when parsed, assert that the result is empty (deterministic: no default guesswork)", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("base-branch:"); got != "" {
			t.Fatalf("base branch = %q, want empty", got)
		}
	})

	t.Run("Given a marker whose value is not a single branch token, when parsed, assert that the result is empty (a branch name never contains whitespace)", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("base-branch: not a branch"); got != "" {
			t.Fatalf("base branch = %q, want empty", got)
		}
	})

	t.Run("Given several base-branch lines, when parsed, assert that the first one wins (deterministic)", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("base-branch: first\ncode\nbase-branch: second"); got != "first" {
			t.Fatalf("base branch = %q, want first", got)
		}
	})

	t.Run("Given a body that merely mentions base-branch mid-sentence, when parsed, assert that it is not mistaken for the directive", func(t *testing.T) {
		if got := intake.BaseBranchFromBody("the base-branch: is documented in the wiki"); got != "" {
			t.Fatalf("base branch = %q, want empty", got)
		}
	})
}
