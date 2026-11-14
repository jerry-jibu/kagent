package gate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jerry-jibu/kagent/orchestrator/internal/gate"
	"github.com/jerry-jibu/kagent/orchestrator/internal/intake"
	"github.com/jerry-jibu/kagent/orchestrator/internal/policy"
)

func testPolicy() policy.Policy {
	p := policy.Policy{
		Repositories: []string{"acme/api", "acme/widgets"},
		Admission: []policy.AdmissionRule{
			{ID: "alice", Strength: policy.StrengthExplicit},
			{ID: "bob", Strength: policy.StrengthInferred},
			{ID: "mallory", Strength: policy.StrengthDenied},
		},
		Bots: []string{"acme-bot"},
	}
	p.Version = p.Hash()
	return p
}

func comment(author, body, repo string) intake.CommentPayload {
	return intake.CommentPayload{
		Kind:      "issue-comment",
		Repo:      repo,
		Issue:     42,
		CommentID: 1001,
		Author:    author,
		Body:      body,
		URL:       "https://github.com/" + repo + "/issues/42#issuecomment-1001",
		CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
	}
}

func TestJudge(t *testing.T) {
	p := testPolicy()

	t.Run("Given an allowlisted repository and a comment by an explicit sender that addresses the bot, when the gate judges, assert that it allows with the explicit strength", func(t *testing.T) {
		v := gate.Judge(p, comment("alice", "@acme-bot please fix the flaky login test", "acme/api"))
		if !v.Allowed {
			t.Fatalf("verdict must allow, got %+v", v)
		}
		if v.Strength != policy.StrengthExplicit {
			t.Fatalf("strength = %q, want explicit", v.Strength)
		}
	})

	t.Run("Given an allowlisted repository and a comment by an inferred sender that addresses the bot, when the gate judges, assert that it allows with the inferred strength from the admission table", func(t *testing.T) {
		v := gate.Judge(p, comment("bob", "@acme-bot take a look at this", "acme/widgets"))
		if !v.Allowed || v.Strength != policy.StrengthInferred {
			t.Fatalf("verdict = %+v, want allowed with inferred strength", v)
		}
	})

	t.Run("Given a comment in a repository outside the allowlist, when the gate judges, assert that it denies with the allowlist reason and no strength", func(t *testing.T) {
		v := gate.Judge(p, comment("alice", "@acme-bot please fix", "acme/secret"))
		if v.Allowed {
			t.Fatalf("verdict must deny, got %+v", v)
		}
		if v.Strength != "" {
			t.Fatalf("denial must carry no strength, got %q", v.Strength)
		}
		if !strings.Contains(v.Reason, "allowlist") {
			t.Fatalf("reason %q must name the allowlist", v.Reason)
		}
	})

	t.Run("Given a sender that is not in the admission table, when the gate judges, assert that it denies by default", func(t *testing.T) {
		v := gate.Judge(p, comment("eve", "@acme-bot please delete the database", "acme/api"))
		if v.Allowed {
			t.Fatalf("verdict must deny an unlisted sender, got %+v", v)
		}
		if !strings.Contains(v.Reason, "eve") {
			t.Fatalf("reason %q must name the refused sender", v.Reason)
		}
	})

	t.Run("Given a sender whose admission strength is denied, when the gate judges, assert that it denies", func(t *testing.T) {
		v := gate.Judge(p, comment("mallory", "@acme-bot please fix", "acme/api"))
		if v.Allowed {
			t.Fatalf("verdict must deny a denied sender, got %+v", v)
		}
	})

	t.Run("Given a comment authored by a bot, when the gate judges, assert that it denies because bot comments never count as human triggers", func(t *testing.T) {
		v := gate.Judge(p, comment("acme-bot", "@acme-bot self-follow-up", "acme/api"))
		if v.Allowed {
			t.Fatalf("verdict must deny a bot author, got %+v", v)
		}
		if !strings.Contains(v.Reason, "bot") {
			t.Fatalf("reason %q must name the bot rule", v.Reason)
		}
	})

	t.Run("Given a comment that does not address any policy bot, when the gate judges, assert that it denies so background conversation never reaches the evaluator", func(t *testing.T) {
		v := gate.Judge(p, comment("alice", "has anyone seen the flaky login test?", "acme/api"))
		if v.Allowed {
			t.Fatalf("verdict must deny an unaddressed comment, got %+v", v)
		}
	})

	t.Run("Given a comment whose mention is a prefix of a different login, when the gate judges, assert that it denies because the mention does not address the bot", func(t *testing.T) {
		v := gate.Judge(p, comment("alice", "@acme-bot2 please fix", "acme/api"))
		if v.Allowed {
			t.Fatalf("verdict must deny, got %+v", v)
		}
	})

	t.Run("Given a comment that addresses the bot in mixed case, when the gate judges, assert that it still allows because GitHub mentions match case-insensitively", func(t *testing.T) {
		v := gate.Judge(p, comment("alice", "hey @ACME-Bot please fix", "acme/api"))
		if !v.Allowed || v.Strength != policy.StrengthExplicit {
			t.Fatalf("verdict = %+v, want allowed with explicit strength", v)
		}
	})
}
